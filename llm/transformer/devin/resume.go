// 职责: Devin GetChatMessage 流被上游截断后按原请求重订阅续传。
// 关联文件:
//
//	llm/transformer/devin/outbound.go
//	llm/transformer/devin/stream.go
//	llm/transformer/devin/wire.go
//
// 注意事项:
//
//	重发同一 GetChatMessage 请求会让上游回放或继续当前生成；已交付帧按前缀去重。
//	回放中途分叉、或新流携带不同 OutputID，说明上游重新生成，不能拼接两条生成。
package devin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/streams"
)

const (
	// maxResumeEmpty mirrors the CLI's generous reconnect budget while the
	// stream has not produced any frame yet.
	maxResumeEmpty = 5
	// maxResumeAfterFrames mirrors the mid-stream restart budget: once
	// frames reached the client a dropped stream is resent at most this
	// many times.
	maxResumeAfterFrames = 3
)

// resumeStream re-subscribes GetChatMessage when the upstream Connect-RPC
// stream ends without its EOS trailer. Resending the identical request
// attaches to the same server-side generation, which either replays the
// frame log from the start or continues with fresh frames; frames already
// delivered downstream are skipped by prefix matching.
type resumeStream struct {
	ctx      context.Context
	exec     pipeline.Executor
	req      *httpclient.Request
	source   streams.Stream[*httpclient.StreamEvent]
	emitted  []*httpclient.StreamEvent
	frames   int
	outputID string
	skip     int
	replay   bool
	resumes  int
	terminal bool
	current  *httpclient.StreamEvent
	err      error
}

func (s *resumeStream) Next() bool {
	if s.err != nil {
		return false
	}
	for {
		for s.source.Next() {
			ev := s.source.Current()
			if ev == nil {
				continue
			}
			if s.replay {
				switch s.replayed(ev) {
				case replaySkip:
					continue
				case replayFail:
					return false
				}
			}
			if ev.Type == endEvent {
				s.terminal = true
			} else {
				s.frames++
				if s.outputID == "" {
					s.outputID = frameOutputID(ev)
				}
			}
			s.emitted = append(s.emitted, ev)
			s.current = ev
			return true
		}
		if s.terminal {
			s.err = s.source.Err()
			return false
		}
		if !s.resume() {
			return false
		}
	}
}

// Replay outcomes for one event arriving on a resumed stream.
const (
	replaySkip = iota // event matches the delivered prefix — drop it
	replayPass        // replay phase is over — deliver the event
	replayFail        // diverged generation — abort the stream
)

// replayed resolves one event arriving on a resumed stream against the
// already delivered prefix.
func (s *resumeStream) replayed(ev *httpclient.StreamEvent) int {
	if s.skip < len(s.emitted) && sameStreamEvent(ev, s.emitted[s.skip]) {
		s.skip++
		return replaySkip
	}
	s.replay = false
	if s.skip == len(s.emitted) {
		// The replay caught up to the truncation point.
		return replayPass
	}
	if s.skip > 0 {
		// The server replayed a prefix then diverged: the generation is
		// being rewritten and old deltas cannot be merged with new ones.
		s.err = fmt.Errorf("%w: Devin replay diverged", llm.ErrStreamIncomplete)
		return replayFail
	}
	// No replay at all. A trailer arriving immediately after frames were
	// delivered cannot prove the generation finished — the missing tail
	// would be marked complete silently.
	if ev.Type == endEvent {
		s.err = fmt.Errorf("%w: Devin closed the resumed stream", llm.ErrStreamIncomplete)
		return replayFail
	}
	// Either the generation continues where it stopped or the server
	// started a fresh one. A different output identity means the latter,
	// which cannot be merged into the delivered deltas.
	if id := frameOutputID(ev); s.outputID != "" && id != "" && id != s.outputID {
		s.err = fmt.Errorf("%w: Devin restarted the generation", llm.ErrStreamIncomplete)
		return replayFail
	}
	return replayPass
}

// resume re-issues the identical GetChatMessage request on a fresh stream
// and swaps it in. Upstream API errors and cancellation are not resumed.
func (s *resumeStream) resume() bool {
	srcErr := s.source.Err()
	if s.ctx.Err() != nil {
		s.err = s.ctx.Err()
		return false
	}
	var apiErr *llm.ResponseError
	if errors.As(srcErr, &apiErr) {
		s.err = srcErr
		return false
	}
	budget := maxResumeEmpty
	if s.frames > 0 {
		budget = maxResumeAfterFrames
	}
	if s.resumes >= budget {
		s.err = srcErr
		if s.err == nil {
			s.err = fmt.Errorf("%w: Devin EOS trailer is missing", llm.ErrStreamIncomplete)
		}
		return false
	}
	if !sleepWithContext(s.ctx, resumeBackoff(s.resumes)) {
		s.err = s.ctx.Err()
		return false
	}
	next, err := s.exec.DoStream(s.ctx, s.req)
	if err != nil {
		s.err = fmt.Errorf("Devin resume request failed: %w", err)
		return false
	}
	slog.InfoContext(s.ctx, "devin stream resume: resubscribing",
		slog.Int("resume", s.resumes+1), slog.Int("frames", s.frames))
	_ = s.source.Close()
	s.source = next
	s.skip = 0
	s.replay = true
	s.resumes++
	return true
}

func (s *resumeStream) Current() *httpclient.StreamEvent { return s.current }
func (s *resumeStream) Err() error                       { return s.err }

func (s *resumeStream) Close() error {
	if s.source == nil {
		return nil
	}
	return s.source.Close()
}

// resumeBackoff mirrors the CLI's reconnect backoff: 100ms * 2^attempt
// clamped to [1s, 10s].
func resumeBackoff(attempt int) time.Duration {
	delay := 100 * time.Millisecond << min(attempt, 6)
	return min(max(delay, time.Second), 10*time.Second)
}

// sleepWithContext waits for the delay or reports false when ctx ends first.
func sleepWithContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func sameStreamEvent(a, b *httpclient.StreamEvent) bool {
	return a != nil && b != nil && a.Type == b.Type && bytes.Equal(a.Data, b.Data)
}

func frameOutputID(ev *httpclient.StreamEvent) string {
	if ev == nil || ev.Type != "devin.frame" {
		return ""
	}
	var payload struct {
		Frame struct {
			OutputID string `json:"OutputID"`
		} `json:"Frame"`
	}
	_ = json.Unmarshal(ev.Data, &payload)
	return payload.Frame.OutputID
}
