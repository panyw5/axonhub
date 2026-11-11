package devin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/samber/lo"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
)

const endEvent = "devin.done"

type framePayload struct {
	Frame                               DevinFrameResult
	Content, Thinking                   []byte
	ToolArguments, InvalidToolArguments [][]byte
}

func init() { httpclient.RegisterDecoder(connectContentType, newDecoder) }

type frameDecoder struct {
	ctx     context.Context
	body    io.ReadCloser
	current *httpclient.StreamEvent
	err     error
	done    bool
}

func newDecoder(ctx context.Context, body io.ReadCloser) httpclient.StreamDecoder {
	return &frameDecoder{ctx: ctx, body: body}
}

func (s *frameDecoder) Next() bool {
	if s.done || s.err != nil {
		return false
	}
	if s.err = s.ctx.Err(); s.err != nil {
		return false
	}
	flag, data, err := ReadConnectFrame(s.body)
	if err != nil {
		s.err = err
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			s.err = fmt.Errorf("%w: Devin EOS trailer is missing", llm.ErrStreamIncomplete)
		}
		return false
	}
	if flag&ConnectFlagEndStream != 0 {
		var trailer map[string]json.RawMessage
		if err := json.Unmarshal(data, &trailer); err != nil || trailer == nil {
			s.err = fmt.Errorf("invalid Devin EOS trailer")
			return false
		}
		if raw, ok := trailer["error"]; ok && string(raw) != "null" {
			var detail struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			if json.Unmarshal(raw, &detail) != nil || detail.Code == "" {
				s.err = fmt.Errorf("invalid Devin trailer error")
				return false
			}
		}
		if code, err := ParseDevinTrailerError(data); err != nil {
			s.err = &llm.ResponseError{StatusCode: code, Detail: llm.ErrorDetail{Type: "api_error", Message: err.Error()}, Cause: err}
			return false
		}
		s.done = true
		s.current = &httpclient.StreamEvent{Type: endEvent, Data: []byte("{}")}
		return true
	}
	frame, err := ParseDevinFrame(data)
	if err != nil {
		s.err = fmt.Errorf("decode Devin frame: %w", err)
		return false
	}
	// Preserve split UTF-8 as bytes until the response state can join chunks.
	wire := framePayload{Frame: frame, Content: []byte(frame.ContentText), Thinking: []byte(frame.ThinkingText)}
	for _, tool := range frame.ToolCallDeltas {
		wire.ToolArguments = append(wire.ToolArguments, []byte(tool.Arguments))
		wire.InvalidToolArguments = append(wire.InvalidToolArguments, []byte(tool.InvalidJSONStr))
	}
	payload, err := json.Marshal(wire)
	if err != nil {
		s.err = err
		return false
	}
	s.current = &httpclient.StreamEvent{Type: "devin.frame", Data: payload}
	return true
}

func (s *frameDecoder) Current() *httpclient.StreamEvent { return s.current }
func (s *frameDecoder) Err() error                       { return s.err }
func (s *frameDecoder) Close() error                     { s.done = true; return s.body.Close() }

type responseState struct {
	id, model                  string
	created                    int64
	content, thinking          strings.Builder
	contentTail, thinkingTail  []byte
	signature                  []byte
	signatureType              string
	tools                      []llm.ToolCall
	toolIDs                    map[string]int
	toolTails                  map[int][]byte
	activeTool                 int
	usage                      DevinUsage
	stop                       uint64
	done, started, hasThinking bool
	pending                    []*llm.Response
}

func newResponseState(req *httpclient.Request) *responseState {
	s := &responseState{id: "chatcmpl-" + uuid.NewString(), created: time.Now().Unix(), toolIDs: make(map[string]int), toolTails: make(map[int][]byte), activeTool: -1}
	if req != nil {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(req.Body, &body)
		s.model = body.Model
	}
	return s
}

func (s *responseState) chunk(delta *llm.Message) *llm.Response {
	return &llm.Response{ID: s.id, Model: s.model, Created: s.created, Object: "chat.completion.chunk", APIFormat: llm.APIFormatDevinChat, RequestType: llm.RequestTypeChat, Choices: []llm.Choice{{Index: 0, Delta: delta}}}
}

func completeUTF8(tail *[]byte, data []byte) string {
	joined := append(*tail, data...)
	end := 0
	for end < len(joined) && utf8.FullRune(joined[end:]) {
		_, n := utf8.DecodeRune(joined[end:])
		end += n
	}
	value := string(joined[:end])
	*tail = append([]byte(nil), joined[end:]...)
	return value
}

func (s *responseState) consume(event *httpclient.StreamEvent) ([]*llm.Response, error) {
	if event == nil || s.done {
		return nil, nil
	}
	var out []*llm.Response
	if event.Type == endEvent {
		if len(s.contentTail) != 0 || len(s.thinkingTail) != 0 {
			return nil, fmt.Errorf("Devin stream contains incomplete UTF-8")
		}
		for _, tool := range s.tools {
			if len(s.toolTails[tool.Index]) != 0 || tool.ID == "" || tool.Function.Name == "" || !json.Valid([]byte(tool.Function.Arguments)) {
				return nil, fmt.Errorf("Devin returned an incomplete or invalid function call")
			}
		}
		s.done = true
		if !s.started {
			s.started = true
			out = append(out, s.chunk(&llm.Message{Role: "assistant"}))
		}
		if len(s.signature) > 0 {
			out = append(out, s.chunk(&llm.Message{ReasoningSignature: lo.ToPtr(encodeSignature(s.signature, s.signatureType))}))
		}
		out = append(out, s.pending...)
		s.pending = nil
		finish := s.chunk(&llm.Message{})
		finish.Choices[0].FinishReason = lo.ToPtr(s.finishReason())
		finish.Usage = s.unifiedUsage()
		out = append(out, finish, llm.DoneResponse)
		return out, nil
	}
	var wire framePayload
	if err := json.Unmarshal(event.Data, &wire); err != nil {
		return nil, err
	}
	f := wire.Frame
	if !s.started && f.OutputID != "" {
		s.id = f.OutputID
	}
	if !s.started && f.Timestamp > 0 {
		s.created = int64(f.Timestamp)
	}
	if !s.started {
		s.started = true
		out = append(out, s.chunk(&llm.Message{Role: "assistant"}))
	}
	if f.StopReason != 0 {
		s.stop = f.StopReason
	}
	if f.Usage != nil {
		u := f.Usage
		if u.StatusCode >= 400 {
			return nil, &llm.ResponseError{StatusCode: int(u.StatusCode), Detail: llm.ErrorDetail{Type: "api_error", Message: "Devin model request failed"}}
		}
		if u.PromptTokens != 0 {
			s.usage.PromptTokens = u.PromptTokens
		}
		if u.CompletionTokens != 0 {
			s.usage.CompletionTokens = u.CompletionTokens
		}
		if u.CachedTokens != 0 {
			s.usage.CachedTokens = u.CachedTokens
		}
		if u.CacheWriteTokens != 0 {
			s.usage.CacheWriteTokens = u.CacheWriteTokens
		}
	}
	if in, output, cached, ok := ParseDevinResponseDimensionGroups(f.ResponseDimensionGroups...); ok {
		if s.usage.PromptTokens == 0 {
			s.usage.PromptTokens = in
		}
		if s.usage.CompletionTokens == 0 {
			s.usage.CompletionTokens = output
		}
		if s.usage.CachedTokens == 0 {
			s.usage.CachedTokens = cached
		}
	}
	s.signature = append(s.signature, f.DeltaSignature...)
	if f.DeltaSignatureType != "" {
		s.signatureType = f.DeltaSignatureType
	}
	if text := completeUTF8(&s.thinkingTail, wire.Thinking); text != "" {
		s.hasThinking = true
		s.thinking.WriteString(text)
		out = append(out, s.chunk(&llm.Message{ReasoningContent: lo.ToPtr(text)}))
	}
	var actions []*llm.Response
	for i, tc := range f.ToolCallDeltas {
		index, exists := s.toolIDs[tc.ID]
		if tc.ID == "" && s.activeTool >= 0 {
			index, exists = s.activeTool, true
		}
		if !exists && s.activeTool >= 0 && s.tools[s.activeTool].ID == "" {
			index, exists = s.activeTool, true
		}
		if !exists {
			if len(s.tools) >= 128 {
				return nil, fmt.Errorf("Devin tool call limit exceeded")
			}
			index = len(s.tools)
			s.tools = append(s.tools, llm.ToolCall{Index: index, Type: "function"})
		}
		s.activeTool = index
		call := &s.tools[index]
		delta := llm.ToolCall{Index: index}
		if !exists {
			delta.Type = "function"
		}
		if tc.ID != "" {
			s.toolIDs[tc.ID] = index
			if call.ID == "" {
				call.ID = encodeToolID(tc.ID)
				delta.ID = call.ID
			}
		}
		if tc.Name != "" && call.Function.Name == "" {
			call.Function.Name = tc.Name
			delta.Function.Name = tc.Name
		}
		if i >= len(wire.ToolArguments) || i >= len(wire.InvalidToolArguments) {
			return nil, fmt.Errorf("invalid Devin tool argument frame")
		}
		rawArgs := wire.ToolArguments[i]
		if len(rawArgs) == 0 {
			rawArgs = wire.InvalidToolArguments[i]
		}
		tail := s.toolTails[index]
		args := completeUTF8(&tail, rawArgs)
		s.toolTails[index] = tail
		call.Function.Arguments += args
		delta.Function.Arguments = args
		actions = append(actions, s.chunk(&llm.Message{ToolCalls: []llm.ToolCall{delta}}))
	}
	if text := completeUTF8(&s.contentTail, wire.Content); text != "" {
		s.content.WriteString(text)
		actions = append(actions, s.chunk(&llm.Message{Content: llm.MessageContent{Content: lo.ToPtr(text)}}))
	}
	// Block protocols require a complete thought signature before switching to tools/text.
	if s.hasThinking {
		s.pending = append(s.pending, actions...)
	} else {
		out = append(out, actions...)
	}
	return out, nil
}

func (s *responseState) finishReason() string {
	switch s.stop {
	case 1, 3:
		return "length"
	case 11:
		return "content_filter"
	case 10:
		return "tool_calls"
	}
	if len(s.tools) > 0 {
		return "tool_calls"
	}
	return "stop"
}

func (s *responseState) unifiedUsage() *llm.Usage {
	input := s.usage.PromptTokens + s.usage.CachedTokens + s.usage.CacheWriteTokens
	return &llm.Usage{PromptTokens: input, CompletionTokens: s.usage.CompletionTokens, TotalTokens: input + s.usage.CompletionTokens, PromptTokensDetails: &llm.PromptTokensDetails{CachedTokens: s.usage.CachedTokens, WriteCachedTokens: s.usage.CacheWriteTokens}}
}

func (s *responseState) response() *llm.Response {
	msg := &llm.Message{Role: "assistant", Content: llm.MessageContent{Content: lo.ToPtr(s.content.String())}, ToolCalls: s.tools}
	if s.thinking.Len() > 0 {
		msg.ReasoningContent = lo.ToPtr(s.thinking.String())
	}
	if len(s.signature) > 0 {
		msg.ReasoningSignature = lo.ToPtr(encodeSignature(s.signature, s.signatureType))
	}
	return &llm.Response{ID: s.id, Model: s.model, Created: s.created, Object: "chat.completion", APIFormat: llm.APIFormatDevinChat, RequestType: llm.RequestTypeChat, Usage: s.unifiedUsage(), Choices: []llm.Choice{{Index: 0, Message: msg, FinishReason: lo.ToPtr(s.finishReason())}}}
}

type responseStream struct {
	source  streams.Stream[*httpclient.StreamEvent]
	state   *responseState
	queue   []*llm.Response
	current *llm.Response
	err     error
}

func (*OutboundTransformer) TransformStream(_ context.Context, req *httpclient.Request, source streams.Stream[*httpclient.StreamEvent]) (streams.Stream[*llm.Response], error) {
	return &responseStream{source: source, state: newResponseState(req)}, nil
}

func (s *responseStream) Next() bool {
	if s.err != nil {
		return false
	}
	for len(s.queue) == 0 {
		if s.state.done {
			return false
		}
		if !s.source.Next() {
			s.err = s.source.Err()
			if s.err == nil {
				s.err = llm.ErrStreamIncomplete
			}
			return false
		}
		s.queue, s.err = s.state.consume(s.source.Current())
		if s.err != nil {
			return false
		}
	}
	s.current = s.queue[0]
	s.queue = s.queue[1:]
	return true
}
func (s *responseStream) Current() *llm.Response { return s.current }
func (s *responseStream) Err() error             { return s.err }
func (s *responseStream) Close() error           { return s.source.Close() }

func (*OutboundTransformer) AggregateStreamChunks(_ context.Context, req *httpclient.Request, chunks []*httpclient.StreamEvent) ([]byte, llm.ResponseMeta, error) {
	state := newResponseState(req)
	for _, chunk := range chunks {
		if _, err := state.consume(chunk); err != nil {
			return nil, llm.ResponseMeta{}, err
		}
	}
	body, err := json.Marshal(state.response())
	return body, llm.ResponseMeta{ID: state.id, Usage: state.unifiedUsage(), Completed: state.done}, err
}
