package commandcode

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/auth"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
)

const (
	defaultBaseURL         = "https://api.commandcode.ai"
	generatePath           = "/alpha/generate"
	commandCodeVersion     = "1.28.1"
	defaultMaxOutputTokens = int64(64_000)
)

type Config struct {
	BaseURL        string
	EndpointPath   string
	APIKeyProvider auth.APIKeyProvider
}

type OutboundTransformer struct {
	config Config
}

type requestEnvelope struct {
	Config   requestConfig `json:"config"`
	Memory   any           `json:"memory"`
	Taste    any           `json:"taste"`
	Skills   any           `json:"skills"`
	Params   requestParams `json:"params"`
	ThreadID string        `json:"threadId"`
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
	Temperature     float64           `json:"temperature"`
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
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	ToolCallID   string          `json:"toolCallId,omitempty"`
	ToolName     string          `json:"toolName,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	Args         json.RawMessage `json:"args,omitempty"`
	Arguments    json.RawMessage `json:"arguments,omitempty"`
	FinishReason string          `json:"finishReason,omitempty"`
	TotalUsage   *providerUsage  `json:"totalUsage,omitempty"`
	Error        any             `json:"error,omitempty"`
	Message      string          `json:"message,omitempty"`
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
	return &OutboundTransformer{config: resolved}, nil
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

	maxTokens := defaultMaxOutputTokens
	if req.MaxCompletionTokens != nil {
		maxTokens = min(*req.MaxCompletionTokens, defaultMaxOutputTokens)
	} else if req.MaxTokens != nil {
		maxTokens = min(*req.MaxTokens, defaultMaxOutputTokens)
	}
	temperature := 0.3
	if req.Temperature != nil {
		temperature = *req.Temperature
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
		Config: requestConfig{
			WorkingDir:    "axonhub",
			Date:          time.Now().UTC().Format(time.DateOnly),
			Environment:   fmt.Sprintf("%s-%s, AxonHub", runtime.GOOS, runtime.GOARCH),
			Structure:     []any{},
			RecentCommits: []any{},
		},
		Params: requestParams{
			Model:           req.Model,
			Messages:        messages,
			Tools:           tools,
			System:          system,
			MaxTokens:       maxTokens,
			Temperature:     temperature,
			Stream:          true,
			ReasoningEffort: req.ReasoningEffort,
		},
		ThreadID: uuid.NewString(),
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
	headers.Set("Accept", "application/jsonl, text/event-stream")
	headers.Set("Authorization", "Bearer "+t.config.APIKeyProvider.Get(ctx))
	headers.Set("X-Command-Code-Version", commandCodeVersion)
	headers.Set("X-Cli-Environment", "production")
	headers.Set("X-Project-Slug", "axonhub")
	headers.Set("X-Taste-Learning", "true")
	headers.Set("X-Co-Flag", "false")

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

func (t *OutboundTransformer) TransformResponse(_ context.Context, response *httpclient.Response) (*llm.Response, error) {
	if response == nil {
		return nil, fmt.Errorf("Command Code response is nil")
	}
	events, err := parseEvents(response.Body)
	if err != nil {
		return nil, err
	}
	for _, event := range events {
		if event.Type == "error" {
			return nil, fmt.Errorf("Command Code stream error: %s", eventErrorMessage(event))
		}
	}
	result := aggregateEvents(events, requestModel(response.Request))
	normalizedBody, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal Command Code response: %w", err)
	}
	response.Body = normalizedBody
	return result, nil
}

func (t *OutboundTransformer) TransformStream(ctx context.Context, req *httpclient.Request, stream streams.Stream[*httpclient.StreamEvent]) (streams.Stream[*llm.Response], error) {
	model := requestModel(req)
	converted := streams.MapErr(stream, func(raw *httpclient.StreamEvent) (*llm.Response, error) {
		if raw == nil || len(raw.Data) == 0 {
			return nil, nil
		}
		event, err := parseEvent(raw.Data)
		if err != nil {
			return nil, err
		}
		return eventResponse(event, model)
	})
	return streams.NoNil(converted), nil
}

func parseEvents(body []byte) ([]streamEvent, error) {
	lines := strings.Split(string(body), "\n")
	events := make([]streamEvent, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") {
			continue
		}
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
	if event.Type == "error" {
		return nil, fmt.Errorf("Command Code stream error: %s", eventErrorMessage(event))
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

func eventErrorMessage(event streamEvent) string {
	if event.Message != "" {
		return event.Message
	}
	if value, ok := event.Error.(string); ok {
		return value
	}
	if value, ok := event.Error.(map[string]any); ok {
		if message, ok := value["message"].(string); ok {
			return message
		}
	}
	return "unknown error"
}

func aggregateEvents(events []streamEvent, model string) *llm.Response {
	var text, reasoning strings.Builder
	var toolCalls []llm.ToolCall
	finishReason := "stop"
	var usage *llm.Usage
	for _, event := range events {
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
			finishReason = normalizeFinishReason(event.FinishReason)
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
		Choices: []llm.Choice{{Index: 0, Message: message, FinishReason: &finishReason}},
	}
}

func normalizeFinishReason(reason string) string {
	switch reason {
	case "tool-calls":
		return "tool_calls"
	case "max_tokens", "max_output_tokens":
		return "length"
	case "":
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

func (t *OutboundTransformer) AggregateStreamChunks(_ context.Context, req *httpclient.Request, chunks []*httpclient.StreamEvent) ([]byte, llm.ResponseMeta, error) {
	events := make([]streamEvent, 0, len(chunks))
	for _, chunk := range chunks {
		if chunk == nil || len(chunk.Data) == 0 {
			continue
		}
		event, err := parseEvent(chunk.Data)
		if err != nil {
			return nil, llm.ResponseMeta{}, err
		}
		if event.Type == "error" {
			return nil, llm.ResponseMeta{}, fmt.Errorf("Command Code stream error: %s", eventErrorMessage(event))
		}
		events = append(events, event)
	}
	response := aggregateEvents(events, requestModel(req))
	body, err := json.Marshal(response)
	if err != nil {
		return nil, llm.ResponseMeta{}, fmt.Errorf("failed to marshal Command Code response: %w", err)
	}
	return body, llm.ResponseMeta{ID: response.ID, Usage: response.Usage, Completed: true}, nil
}

var _ transformer.Outbound = (*OutboundTransformer)(nil)
