package commandcode

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/auth"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
)

const (
	defaultBaseURL       = "https://api.commandcode.ai"
	generatePath         = "/alpha/generate"
	commandCodeUserAgent = "cli"
	// Version mirrors the official command-code CLI release we emulate.
	Version                = "1.89.1"
	defaultMaxOutputTokens = int64(64_000)
	// maxContinuationAttempts mirrors the CLI's pause_turn resend loop (Zr = 5).
	maxContinuationAttempts = 5
	// maxStreamAttempts mirrors the CLI's callModelWithRetry total attempt
	// budget (jr = 10) for failures before any content was emitted.
	maxStreamAttempts = 10
	// maxRestartsAfterContent mirrors the CLI's mid-stream restart budget
	// (Kr = 3): once text reached the client, a dropped stream is resent at
	// most this many times.
	maxRestartsAfterContent = 3
)

type Config struct {
	BaseURL        string
	EndpointPath   string
	APIKeyProvider auth.APIKeyProvider
	// HTTPClient executes the CLI's auxiliary calls (fingerprint record,
	// pause_turn continuation). Continuation and fingerprinting are skipped
	// when nil.
	HTTPClient *httpclient.HttpClient
	// WorkingDir overrides the directory reported in request config. Defaults
	// to the process working directory, like the CLI.
	WorkingDir string
}

type OutboundTransformer struct {
	config       Config
	client       *httpclient.HttpClient
	sessionID    string
	workingDir   string
	projectSlug  string
	configOnce   sync.Once
	cachedConfig requestConfig
	fpOnce       sync.Once
}

type requestEnvelope struct {
	Config         requestConfig `json:"config"`
	Memory         any           `json:"memory"`
	Taste          any           `json:"taste"`
	Skills         any           `json:"skills"`
	PermissionMode string        `json:"permissionMode"`
	ThreadID       string        `json:"threadId"`
	Params         requestParams `json:"params"`
}

type requestConfig struct {
	WorkingDir    string `json:"workingDir"`
	Date          string `json:"date"`
	Environment   string `json:"environment"`
	Structure     []any  `json:"structure"`
	IsGitRepo     bool   `json:"isGitRepo"`
	CurrentBranch string `json:"currentBranch"`
	MainBranch    string `json:"mainBranch"`
	GitStatus     string `json:"gitStatus"`
	RecentCommits []any  `json:"recentCommits"`
}

type requestParams struct {
	Model           string            `json:"model"`
	Messages        []providerMessage `json:"messages"`
	Tools           []providerTool    `json:"tools"`
	System          string            `json:"system"`
	MaxTokens       int64             `json:"max_tokens"`
	Temperature     *float64          `json:"temperature,omitempty"`
	Stream          bool              `json:"stream"`
	ReasoningEffort string            `json:"reasoning_effort,omitempty"`
}

type providerMessage struct {
	Role    string `json:"role"`
	Content []any  `json:"content"`
}

type providerTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

type streamEvent struct {
	Type            string          `json:"type"`
	Text            string          `json:"text,omitempty"`
	ToolCallID      string          `json:"toolCallId,omitempty"`
	ToolName        string          `json:"toolName,omitempty"`
	Input           json.RawMessage `json:"input,omitempty"`
	Args            json.RawMessage `json:"args,omitempty"`
	Arguments       json.RawMessage `json:"arguments,omitempty"`
	FinishReason    string          `json:"finishReason,omitempty"`
	RawFinishReason string          `json:"rawFinishReason,omitempty"`
	TotalUsage      *providerUsage  `json:"totalUsage,omitempty"`
	Error           any             `json:"error,omitempty"`
	Message         string          `json:"message,omitempty"`
	StatusCode      int             `json:"statusCode,omitempty"`
}

type providerUsage struct {
	InputTokens        int64                     `json:"inputTokens"`
	OutputTokens       int64                     `json:"outputTokens"`
	InputTokenDetails  providerInputTokenDetails `json:"inputTokenDetails"`
	CacheWriteTokens1h int64                     `json:"cacheWriteTokens1h"`
}

type providerInputTokenDetails struct {
	CacheReadTokens  int64 `json:"cacheReadTokens"`
	CacheWriteTokens int64 `json:"cacheWriteTokens"`
}

func NewOutboundTransformerWithConfig(config *Config) (transformer.Outbound, error) {
	if config == nil {
		return nil, fmt.Errorf("Command Code config is nil")
	}
	if config.APIKeyProvider == nil {
		return nil, fmt.Errorf("Command Code API key provider is nil")
	}
	resolved := *config
	if strings.TrimSpace(resolved.BaseURL) == "" {
		resolved.BaseURL = defaultBaseURL
	}
	workingDir := resolved.WorkingDir
	if workingDir == "" {
		if cwd, err := os.Getwd(); err == nil {
			workingDir = cwd
		}
	}
	return &OutboundTransformer{
		config:      resolved,
		client:      resolved.HTTPClient,
		sessionID:   uuid.NewString(),
		workingDir:  workingDir,
		projectSlug: projectSlug(workingDir),
	}, nil
}

// serverConfig mirrors the CLI's memoized model-client config: collected once
// per session, with the date refreshed per request.
func (t *OutboundTransformer) serverConfig(ctx context.Context) requestConfig {
	t.configOnce.Do(func() {
		t.cachedConfig = collectServerConfig(ctx, t.workingDir)
	})
	config := t.cachedConfig
	config.Date = time.Now().UTC().Format(time.DateOnly)
	return config
}

func (t *OutboundTransformer) APIFormat() llm.APIFormat {
	return llm.APIFormatOpenAIChatCompletion
}

func (t *OutboundTransformer) TransformRequest(ctx context.Context, req *llm.Request) (*httpclient.Request, error) {
	if req == nil {
		return nil, fmt.Errorf("Command Code request is nil")
	}

	messages, system, err := convertMessages(req.Messages)
	if err != nil {
		return nil, err
	}

	t.fpOnce.Do(func() { t.scheduleFingerprint(ctx) })

	maxTokens := defaultMaxOutputTokens
	if req.MaxCompletionTokens != nil {
		maxTokens = min(*req.MaxCompletionTokens, defaultMaxOutputTokens)
	} else if req.MaxTokens != nil {
		maxTokens = min(*req.MaxTokens, defaultMaxOutputTokens)
	}

	tools := make([]providerTool, 0, len(req.Tools))
	for _, tool := range req.Tools {
		if tool.Type != "function" {
			continue
		}
		tools = append(tools, providerTool{
			Type:        "function",
			Name:        tool.Function.Name,
			Description: tool.Function.Description,
			InputSchema: tool.Function.Parameters,
		})
	}

	body, err := json.Marshal(requestEnvelope{
		Config:         t.serverConfig(ctx),
		PermissionMode: "standard",
		Params: requestParams{
			Model:           req.Model,
			Messages:        messages,
			Tools:           tools,
			System:          system,
			MaxTokens:       maxTokens,
			Temperature:     req.Temperature,
			Stream:          true,
			ReasoningEffort: req.ReasoningEffort,
		},
		ThreadID: t.sessionID,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal Command Code request: %w", err)
	}

	path := t.config.EndpointPath
	if path == "" {
		path = generatePath
	}
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "*/*")
	headers.Set("Accept-Language", "*")
	headers.Set("Sec-Fetch-Mode", "cors")
	headers.Set("User-Agent", commandCodeUserAgent)
	headers.Set("Authorization", "Bearer "+t.config.APIKeyProvider.Get(ctx))
	headers.Set("X-Command-Code-Version", Version)
	headers.Set("X-Cli-Environment", "production")
	headers.Set("X-Project-Slug", t.projectSlug)
	headers.Set("X-Taste-Learning", "true")
	headers.Set("X-Session-Id", t.sessionID)
	headers.Set("traceparent", newTraceparent())

	return &httpclient.Request{
		Method:      http.MethodPost,
		URL:         strings.TrimRight(t.config.BaseURL, "/") + path,
		Path:        path,
		Headers:     headers,
		ContentType: "application/json",
		Body:        body,
		RequestType: string(llm.RequestTypeChat),
		APIFormat:   llm.APIFormatOpenAIChatCompletion.String(),
		Metadata: map[string]string{
			httpclient.MetadataStreamDecoderContentType: "application/jsonl",
		},
	}, nil
}

func convertMessages(messages []llm.Message) ([]providerMessage, string, error) {
	toolCalls := make(map[string]string)
	toolResults := make(map[string]struct{})
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			toolCalls[call.ID] = call.Function.Name
		}
		if message.Role == "tool" && message.ToolCallID != nil {
			toolResults[*message.ToolCallID] = struct{}{}
		}
	}

	var system []string
	result := make([]providerMessage, 0, len(messages))
	for _, message := range messages {
		switch message.Role {
		case "system", "developer":
			system = append(system, contentText(message.Content))
		case "user":
			parts, err := userContent(message.Content)
			if err != nil {
				return nil, "", err
			}
			result = append(result, providerMessage{Role: "user", Content: parts})
		case "assistant":
			parts := make([]any, 0, 2+len(message.ToolCalls))
			if message.ReasoningContent != nil {
				parts = append(parts, map[string]any{"type": "reasoning", "text": *message.ReasoningContent})
			}
			if text := contentText(message.Content); text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": text})
			}
			for _, call := range message.ToolCalls {
				if _, paired := toolResults[call.ID]; !paired {
					continue
				}
				input := map[string]any{}
				_ = json.Unmarshal([]byte(call.Function.Arguments), &input)
				parts = append(parts, map[string]any{
					"type": "tool-call", "toolCallId": call.ID,
					"toolName": call.Function.Name, "input": input,
				})
			}
			if len(parts) > 0 {
				result = append(result, providerMessage{Role: "assistant", Content: parts})
			}
		case "tool":
			if message.ToolCallID == nil {
				continue
			}
			if _, paired := toolCalls[*message.ToolCallID]; !paired {
				continue
			}
			result = append(result, providerMessage{Role: "tool", Content: []any{map[string]any{
				"type": "tool-result", "toolCallId": *message.ToolCallID,
				"toolName": toolCalls[*message.ToolCallID],
				"output":   map[string]any{"type": "text", "value": contentText(message.Content)},
			}}})
		}
	}
	return result, strings.Join(system, "\n\n"), nil
}

func contentText(content llm.MessageContent) string {
	if len(content.MultipleContent) > 0 {
		var text strings.Builder
		for _, part := range content.MultipleContent {
			if part.Type == "text" && part.Text != nil {
				text.WriteString(*part.Text)
			}
		}
		return text.String()
	}
	if content.Content != nil {
		return *content.Content
	}
	return ""
}

func userContent(content llm.MessageContent) ([]any, error) {
	if len(content.MultipleContent) == 0 {
		return []any{map[string]any{"type": "text", "text": contentText(content)}}, nil
	}
	parts := make([]any, 0, len(content.MultipleContent))
	for _, part := range content.MultipleContent {
		switch part.Type {
		case "text":
			parts = append(parts, map[string]any{"type": "text", "text": contentText(llm.MessageContent{MultipleContent: []llm.MessageContentPart{part}})})
		case "image_url":
			if part.ImageURL == nil {
				return nil, fmt.Errorf("Command Code image input has no URL")
			}
			mediaType, data, ok := strings.Cut(strings.TrimPrefix(part.ImageURL.URL, "data:"), ";base64,")
			if !ok || mediaType == "" {
				return nil, fmt.Errorf("Command Code only supports base64 data URL image input")
			}
			if _, err := base64.StdEncoding.DecodeString(data); err != nil {
				return nil, fmt.Errorf("invalid Command Code base64 image input: %w", err)
			}
			parts = append(parts, map[string]any{"type": "image", "source": map[string]any{
				"type": "base64", "media_type": mediaType, "data": data,
			}})
		default:
			return nil, fmt.Errorf("Command Code does not support input type %q", part.Type)
		}
	}
	return parts, nil
}

func (t *OutboundTransformer) TransformResponse(ctx context.Context, response *httpclient.Response) (*llm.Response, error) {
	if response == nil {
		return nil, fmt.Errorf("Command Code response is nil")
	}
	events, err := t.parseEventsWithContinuation(ctx, response.Request, response.Body)
	if err != nil {
		return nil, err
	}
	result, completed, err := aggregateEvents(events, requestModel(response.Request))
	if err != nil {
		return nil, err
	}
	if !completed {
		return nil, fmt.Errorf("Command Code response missing finish event: %w", llm.ErrStreamIncomplete)
	}
	normalizedBody, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal Command Code response: %w", err)
	}
	response.Body = normalizedBody
	return result, nil
}

// parseEventsWithContinuation mirrors the CLI's pause_turn loop: a finish
// event whose raw reason is "pause_turn" is not terminal — the CLI resends the
// same request and appends the next stream's events, up to
// maxContinuationAttempts extra posts. A body that ends without a terminal
// event is the CLI's retryable truncation case — the same request is resent
// and the partial events are discarded, like the CLI's stream_restart handling.
// Without an HTTP client the pause finish is kept and normalizes to "stop".
func (t *OutboundTransformer) parseEventsWithContinuation(ctx context.Context, req *httpclient.Request, body []byte) ([]streamEvent, error) {
	var all []streamEvent
	for attempt := 0; ; attempt++ {
		events, err := parseEvents(body)
		if err != nil {
			return nil, err
		}
		paused, terminal := false, false
		for _, event := range events {
			if event.Type == "finish" && isPauseTurn(event) && t.client != nil && attempt < maxContinuationAttempts {
				paused = true
				continue
			}
			if event.Type == "finish" || event.Type == "error" || event.Type == "abort" {
				terminal = true
			}
			all = append(all, event)
		}
		if !paused && (terminal || t.client == nil || attempt >= maxStreamAttempts-1) {
			return all, nil
		}
		if !paused {
			all = nil
			if !sleepWithContext(ctx, restartBackoff(attempt)) {
				return nil, ctx.Err()
			}
		}
		resp, err := t.client.Do(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("Command Code continuation request failed: %w", err)
		}
		body = resp.Body
	}
}

// isPauseTurn reports whether a finish event signals the CLI's continuation
// protocol rather than the end of the turn.
func isPauseTurn(event streamEvent) bool {
	raw := event.RawFinishReason
	if raw == "" {
		raw = event.FinishReason
	}
	return raw == "pause_turn"
}

func (t *OutboundTransformer) TransformStream(ctx context.Context, req *httpclient.Request, stream streams.Stream[*httpclient.StreamEvent]) (streams.Stream[*llm.Response], error) {
	return &outboundStream{ctx: ctx, client: t.client, req: req, source: stream, model: requestModel(req)}, nil
}

type outboundStream struct {
	ctx      context.Context
	client   *httpclient.HttpClient
	req      *httpclient.Request
	source   streams.Stream[*httpclient.StreamEvent]
	model    string
	attempts int
	restarts int
	emitted  bool
	current  *llm.Response
	finished bool
	done     bool
	err      error
}

// resume re-issues the generate request after a pause_turn finish, like the
// CLI's continuation loop. Returns false when continuation is unavailable or
// exhausted; on a transport failure it records the error.
func (s *outboundStream) resume() bool {
	if s.client == nil || s.attempts >= maxContinuationAttempts {
		return false
	}
	if !s.resend() {
		return false
	}
	s.attempts++
	return true
}

// restart re-issues the generate request after the upstream stream ended
// without a terminal event, like the CLI's stream_restart handling for
// truncated responses. Once content reached the client the budget drops to
// maxRestartsAfterContent; restarts wait restartBackoff like the CLI.
func (s *outboundStream) restart() bool {
	if s.client == nil {
		slog.WarnContext(s.ctx, "commandcode stream restart skipped: no http client")
		return false
	}
	if s.emitted && s.restarts >= maxRestartsAfterContent {
		return false
	}
	if s.restarts >= maxStreamAttempts-1 {
		return false
	}
	if !sleepWithContext(s.ctx, restartBackoff(s.restarts)) {
		s.err = s.ctx.Err()
		return false
	}
	slog.InfoContext(s.ctx, "commandcode stream restart: resending request",
		slog.Int("restart", s.restarts+1), slog.Bool("emitted", s.emitted))
	if !s.resend() {
		return false
	}
	s.restarts++
	return true
}

// resend posts the generate request again on a fresh stream and swaps it in.
func (s *outboundStream) resend() bool {
	next, err := s.client.DoStream(s.ctx, s.req)
	if err != nil {
		s.err = fmt.Errorf("Command Code continuation request failed: %w", err)
		return false
	}
	_ = s.source.Close()
	s.source = next
	return true
}

// restartBackoff mirrors the CLI's backoffMs: 100ms * 2^attempt clamped to
// [1s, 10s].
func restartBackoff(attempt int) time.Duration {
	delay := 100 * time.Millisecond << min(attempt, 6)
	if delay < time.Second {
		return time.Second
	}
	if delay > 10*time.Second {
		return 10 * time.Second
	}
	return delay
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

func (s *outboundStream) Next() bool {
	if s.done {
		return false
	}
	for s.err == nil {
		for s.source.Next() {
			raw := s.source.Current()
			if raw == nil {
				continue
			}
			event, err := parseEvent(raw.Data)
			if err != nil {
				s.err = err
				return false
			}
			if event.Type == "finish" && isPauseTurn(event) {
				if s.resume() {
					continue
				}
				if s.err != nil {
					return false
				}
			}
			response, err := eventResponse(event, s.model)
			if err != nil {
				s.err = err
				return false
			}
			if response != nil {
				s.finished = s.finished || event.Type == "finish"
				s.emitted = s.emitted || hasDeltaContent(response)
				s.current = response
				return true
			}
		}
		if s.finished {
			s.err = s.source.Err()
			if s.err != nil {
				return false
			}
			// Command Code streams end with a finish event and no [DONE]
			// marker — emit the shared sentinel so downstream terminal
			// detection recognizes a completed stream.
			s.done = true
			s.current = llm.DoneResponse
			return true
		}
		// Stream ended without a finish event — the CLI treats this as a
		// retryable truncation and resends the same request (stream_restart).
		if s.restart() {
			continue
		}
		if s.err == nil {
			if srcErr := s.source.Err(); srcErr != nil {
				s.err = srcErr
			} else {
				s.err = fmt.Errorf("Command Code stream missing finish event: %w", llm.ErrStreamIncomplete)
			}
		}
		slog.WarnContext(s.ctx, "commandcode stream ended without restart",
			slog.Bool("client_nil", s.client == nil),
			slog.Int("restarts", s.restarts),
			slog.Bool("emitted", s.emitted),
			slog.Any("error", s.err),
		)
		return false
	}
	return false
}

// hasDeltaContent reports whether the chunk carries visible output (text,
// reasoning, or tool calls), matching the CLI's onTextDelta tracking for
// mid-stream restart budgeting.
func hasDeltaContent(resp *llm.Response) bool {
	for _, choice := range resp.Choices {
		delta := choice.Delta
		if delta == nil {
			continue
		}
		if len(delta.ToolCalls) > 0 || delta.ReasoningContent != nil ||
			delta.Content.Content != nil || len(delta.Content.MultipleContent) > 0 {
			return true
		}
	}
	return false
}

func (s *outboundStream) Current() *llm.Response { return s.current }
func (s *outboundStream) Err() error             { return s.err }
func (s *outboundStream) Close() error           { return s.source.Close() }

// ExpectedStreamChoices reports the single-choice protocol so finish_reason
// chunks count toward terminal detection before the DoneResponse sentinel.
func (s *outboundStream) ExpectedStreamChoices() int { return 1 }

func parseEvents(body []byte) ([]streamEvent, error) {
	lines := strings.Split(string(body), "\n")
	events := make([]streamEvent, 0, len(lines))
	for _, line := range lines {
		event, err := parseEvent([]byte(line))
		if err != nil {
			return nil, err
		}
		if event.Type != "" {
			events = append(events, event)
		}
	}
	return events, nil
}

func parseEvent(data []byte) (streamEvent, error) {
	line := strings.TrimSpace(string(data))
	switch field, _, _ := strings.Cut(line, ":"); field {
	case "", "event", "id", "retry":
		return streamEvent{}, nil
	}
	line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if line == "" || line == "[DONE]" {
		return streamEvent{}, nil
	}
	var event streamEvent
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		return streamEvent{}, fmt.Errorf("failed to unmarshal Command Code stream event: %w", err)
	}
	return event, nil
}

func eventResponse(event streamEvent, model string) (*llm.Response, error) {
	if err := eventFailure(event); err != nil {
		return nil, err
	}
	if event.Type == "" || event.Type == "start" || event.Type == "reasoning-start" || event.Type == "reasoning-end" {
		return nil, nil
	}

	response := &llm.Response{ID: "commandcode", Object: "chat.completion.chunk", Model: model}
	choice := llm.Choice{Index: 0, Delta: &llm.Message{Role: "assistant"}}
	switch event.Type {
	case "text-delta":
		choice.Delta.Content.Content = &event.Text
	case "reasoning-delta":
		choice.Delta.ReasoningContent = &event.Text
	case "tool-call":
		arguments := eventInput(event)
		choice.Delta.ToolCalls = []llm.ToolCall{{
			ID: event.ToolCallID, Type: "function", Index: 0,
			Function: llm.FunctionCall{Name: event.ToolName, Arguments: arguments},
		}}
	case "finish":
		reason := normalizeFinishReason(event.FinishReason)
		choice.FinishReason = &reason
		response.Usage = convertUsage(event.TotalUsage)
	default:
		return nil, nil
	}
	response.Choices = []llm.Choice{choice}
	return response, nil
}

func eventInput(event streamEvent) string {
	raw := event.Input
	if len(raw) == 0 {
		raw = event.Args
	}
	if len(raw) == 0 {
		raw = event.Arguments
	}
	if len(raw) == 0 || string(raw) == "null" {
		return "{}"
	}
	if raw[0] == '"' {
		var value string
		if json.Unmarshal(raw, &value) == nil {
			return value
		}
	}
	return string(raw)
}

func eventFailure(event streamEvent) error {
	switch event.Type {
	case "abort":
		return &llm.ResponseError{
			StatusCode: http.StatusBadGateway,
			Detail:     llm.ErrorDetail{Type: "commandcode_error", Message: "Command Code aborted the stream"},
		}
	case "error":
	default:
		return nil
	}
	status := event.StatusCode
	detail := llm.ErrorDetail{Type: "commandcode_error", Message: event.Message}
	switch value := event.Error.(type) {
	case string:
		if value != "" {
			detail.Message = value
		}
	case map[string]any:
		if message, ok := value["message"].(string); ok && message != "" {
			detail.Message = message
		}
		if code, ok := value["statusCode"].(float64); ok && code >= 400 && code <= 599 && code == float64(int(code)) {
			status = int(code)
		}
	}
	// The CLI also recognizes errors encoded as a status followed by a JSON envelope.
	if start := strings.IndexByte(detail.Message, '{'); start >= 0 {
		prefix := detail.Message[:start]
		var embedded struct {
			Error llm.ErrorDetail `json:"error"`
		}
		if json.Unmarshal([]byte(detail.Message[start:]), &embedded) == nil && embedded.Error.Message != "" {
			detail.Message = embedded.Error.Message
			detail.Code = embedded.Error.Code
			if embedded.Error.Type != "" {
				detail.Type = embedded.Error.Type
			}
			if code, err := strconv.Atoi(strings.TrimSpace(prefix)); err == nil && code >= 400 && code <= 599 {
				status = code
			}
		}
	}
	if detail.Message == "" {
		detail.Message = "Unknown Command Code stream error"
	}
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}
	return &llm.ResponseError{StatusCode: status, Detail: detail}
}

func aggregateEvents(events []streamEvent, model string) (*llm.Response, bool, error) {
	var text, reasoning strings.Builder
	var toolCalls []llm.ToolCall
	var finishReason *string
	var usage *llm.Usage
	for _, event := range events {
		if err := eventFailure(event); err != nil {
			return nil, false, err
		}
		switch event.Type {
		case "text-delta":
			text.WriteString(event.Text)
		case "reasoning-delta":
			reasoning.WriteString(event.Text)
		case "tool-call":
			toolCalls = append(toolCalls, llm.ToolCall{
				ID: event.ToolCallID, Type: "function", Index: len(toolCalls),
				Function: llm.FunctionCall{Name: event.ToolName, Arguments: eventInput(event)},
			})
		case "finish":
			reason := normalizeFinishReason(event.FinishReason)
			finishReason = &reason
			usage = convertUsage(event.TotalUsage)
		}
	}
	content := text.String()
	message := &llm.Message{Role: "assistant", Content: llm.MessageContent{Content: &content}, ToolCalls: toolCalls}
	if value := reasoning.String(); value != "" {
		message.ReasoningContent = &value
	}
	return &llm.Response{
		ID: "commandcode", Object: "chat.completion", Model: model, Usage: usage,
		Choices: []llm.Choice{{Index: 0, Message: message, FinishReason: finishReason}},
	}, finishReason != nil, nil
}

func normalizeFinishReason(reason string) string {
	switch reason {
	case "tool-calls":
		return "tool_calls"
	case "max_tokens", "max_output_tokens":
		return "length"
	case "", "pause_turn", "end_turn":
		return "stop"
	default:
		return reason
	}
}

func convertUsage(usage *providerUsage) *llm.Usage {
	if usage == nil {
		return nil
	}
	result := &llm.Usage{
		PromptTokens: usage.InputTokens, CompletionTokens: usage.OutputTokens,
		TotalTokens: usage.InputTokens + usage.OutputTokens,
	}
	if usage.InputTokenDetails.CacheReadTokens != 0 || usage.InputTokenDetails.CacheWriteTokens != 0 || usage.CacheWriteTokens1h != 0 {
		result.PromptTokensDetails = &llm.PromptTokensDetails{
			CachedTokens:           usage.InputTokenDetails.CacheReadTokens,
			WriteCachedTokens:      usage.InputTokenDetails.CacheWriteTokens,
			WriteCached1HourTokens: usage.CacheWriteTokens1h,
		}
		if result.PromptTokensDetails.WriteCachedTokens == 0 {
			result.PromptTokensDetails.WriteCachedTokens = usage.CacheWriteTokens1h
		}
	}
	return result
}

func requestModel(req *httpclient.Request) string {
	if req == nil {
		return ""
	}
	var envelope requestEnvelope
	if json.Unmarshal(req.Body, &envelope) == nil {
		return envelope.Params.Model
	}
	return ""
}

func (t *OutboundTransformer) TransformError(_ context.Context, err *httpclient.Error) *llm.ResponseError {
	if err == nil {
		return nil
	}
	return &llm.ResponseError{
		StatusCode: err.StatusCode,
		Detail:     llm.ErrorDetail{Message: string(err.Body), Type: "commandcode_error"},
	}
}

func (t *OutboundTransformer) AggregateStreamChunks(ctx context.Context, req *httpclient.Request, chunks []*httpclient.StreamEvent) ([]byte, llm.ResponseMeta, error) {
	var events []streamEvent
	for attempt := 0; ; attempt++ {
		paused := false
		for _, chunk := range chunks {
			if chunk == nil || len(chunk.Data) == 0 {
				continue
			}
			event, err := parseEvent(chunk.Data)
			if err != nil {
				return nil, llm.ResponseMeta{}, err
			}
			if event.Type == "finish" && isPauseTurn(event) && t.client != nil && attempt < maxContinuationAttempts {
				paused = true
				continue
			}
			events = append(events, event)
		}
		if !paused {
			break
		}
		stream, err := t.client.DoStream(ctx, req)
		if err != nil {
			return nil, llm.ResponseMeta{}, fmt.Errorf("Command Code continuation request failed: %w", err)
		}
		chunks = nil
		for stream.Next() {
			chunks = append(chunks, stream.Current())
		}
		streamErr := stream.Err()
		_ = stream.Close()
		if streamErr != nil {
			return nil, llm.ResponseMeta{}, fmt.Errorf("Command Code continuation stream failed: %w", streamErr)
		}
	}
	response, completed, err := aggregateEvents(events, requestModel(req))
	if err != nil {
		return nil, llm.ResponseMeta{}, err
	}
	body, err := json.Marshal(response)
	if err != nil {
		return nil, llm.ResponseMeta{}, fmt.Errorf("failed to marshal Command Code response: %w", err)
	}
	return body, llm.ResponseMeta{ID: response.ID, Usage: response.Usage, Completed: completed}, nil
}

var _ transformer.Outbound = (*OutboundTransformer)(nil)
