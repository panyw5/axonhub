// 职责: 将统一聊天请求和响应转换为 Mistral Vibe 本地推理使用的 Chat Completions 格式。
// 关联文件:
//
//	llm/transformer/openai/outbound.go
//	internal/server/biz/channel_llm.go
//
// 遵循的 Rules:
//
//	.agents/rules/go-general.md
//
// 注意事项:
//
//	推理历史使用 content 内的 thinking 块。请求透传会绕过消息转换。
package mistral

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/samber/lo"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/auth"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
	"github.com/looplj/axonhub/llm/transformer/openai"
)

const (
	DefaultBaseURL = "https://api.mistral.ai/v1"
	defaultModel   = "mistral-vibe-cli-latest"
)

type OutboundTransformer struct {
	transformer.Outbound
}

func NewOutboundTransformer(baseURL, endpointPath string, keys auth.APIKeyProvider) (*OutboundTransformer, error) {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	base, err := openai.NewOutboundTransformerWithConfig(&openai.Config{
		PlatformType:   openai.PlatformOpenAI,
		BaseURL:        baseURL,
		EndpointPath:   endpointPath,
		APIKeyProvider: keys,
		ReasoningField: openai.ReasoningFieldContent,
	})
	if err != nil {
		return nil, err
	}
	return &OutboundTransformer{Outbound: base}, nil
}

func (*OutboundTransformer) AllowPassThroughBody(context.Context, *llm.Request, *httpclient.Request) bool {
	return false
}

func (*OutboundTransformer) AllowPassThroughResponse(context.Context, *llm.Request, *httpclient.Request) bool {
	return false
}

type chatRequest struct {
	Model             string                 `json:"model"`
	Messages          []chatMessage          `json:"messages"`
	MaxTokens         *int64                 `json:"max_tokens,omitempty"`
	Temperature       *float64               `json:"temperature,omitempty"`
	TopP              *float64               `json:"top_p,omitempty"`
	FrequencyPenalty  *float64               `json:"frequency_penalty,omitempty"`
	PresencePenalty   *float64               `json:"presence_penalty,omitempty"`
	Logprobs          *bool                  `json:"logprobs,omitempty"`
	TopLogprobs       *int64                 `json:"top_logprobs,omitempty"`
	RandomSeed        *int64                 `json:"random_seed,omitempty"`
	Metadata          map[string]string      `json:"metadata,omitempty"`
	ReasoningEffort   string                 `json:"reasoning_effort,omitempty"`
	Stop              *openai.Stop           `json:"stop,omitempty"`
	Stream            *bool                  `json:"stream,omitempty"`
	ParallelToolCalls *bool                  `json:"parallel_tool_calls,omitempty"`
	Tools             []openai.Tool          `json:"tools,omitempty"`
	ToolChoice        *openai.ToolChoice     `json:"tool_choice,omitempty"`
	ResponseFormat    *openai.ResponseFormat `json:"response_format,omitempty"`
}

type chatMessage struct {
	Role       string            `json:"role"`
	Content    any               `json:"content"`
	Name       *string           `json:"name,omitempty"`
	ToolCallID *string           `json:"tool_call_id,omitempty"`
	ToolCalls  []requestToolCall `json:"tool_calls,omitempty"`
}

type requestToolCall struct {
	ID       string              `json:"id,omitempty"`
	Type     string              `json:"type"`
	Function openai.FunctionCall `json:"function"`
}

type contentChunk struct {
	Type     string           `json:"type"`
	Text     *string          `json:"text,omitempty"`
	ImageURL *openai.ImageURL `json:"image_url,omitempty"`
	Thinking []contentChunk   `json:"thinking,omitempty"`
}

func (t *OutboundTransformer) TransformRequest(ctx context.Context, request *llm.Request) (*httpclient.Request, error) {
	if request == nil || (request.RequestType != "" && request.RequestType != llm.RequestTypeChat) {
		return nil, fmt.Errorf("%w: Mistral Vibe requires a chat request", transformer.ErrInvalidRequest)
	}
	out, err := t.Outbound.TransformRequest(ctx, request)
	if err != nil {
		return nil, err
	}

	var source openai.Request
	if err := json.Unmarshal(out.Body, &source); err != nil {
		return nil, fmt.Errorf("decode chat request: %w", err)
	}

	wire := chatRequest{
		Model: source.Model, MaxTokens: source.MaxTokens,
		Temperature: source.Temperature, TopP: source.TopP,
		FrequencyPenalty: source.FrequencyPenalty, PresencePenalty: source.PresencePenalty,
		Logprobs: source.Logprobs, TopLogprobs: source.TopLogprobs, RandomSeed: source.Seed,
		Metadata: source.Metadata, Stop: source.Stop, Stream: source.Stream,
		ParallelToolCalls: source.ParallelToolCalls, Tools: source.Tools, ToolChoice: source.ToolChoice,
		ResponseFormat: source.ResponseFormat,
		Messages:       make([]chatMessage, 0, len(source.Messages)),
	}
	if wire.Temperature == nil {
		wire.Temperature = lo.ToPtr(1.0)
	}
	if source.MaxCompletionTokens != nil {
		wire.MaxTokens = source.MaxCompletionTokens
	}
	if len(wire.Tools) > 0 && wire.ParallelToolCalls == nil {
		wire.ParallelToolCalls = lo.ToPtr(true)
	}
	if wire.ToolChoice != nil && wire.ToolChoice.ToolChoice != nil && *wire.ToolChoice.ToolChoice == "required" {
		wire.ToolChoice = &openai.ToolChoice{ToolChoice: lo.ToPtr("any")}
	}
	// The CLI default route uses high reasoning. For other models, only send
	// a reasoning setting when the caller requests one.
	switch source.ReasoningEffort {
	case "":
		if source.Model == defaultModel {
			wire.ReasoningEffort = "high"
		}
	case "minimal", "low", "medium", "high", "xhigh", "max":
		wire.ReasoningEffort = source.ReasoningEffort
	case "off", "none":
		// The default Vibe route advertises adjustable reasoning. Other
		// models may reject the off value unless their catalogue says so.
		if source.Model == defaultModel {
			wire.ReasoningEffort = "none"
		}
	}
	for _, message := range source.Messages {
		converted, err := convertMessage(message, wire.ReasoningEffort != "" && wire.ReasoningEffort != "none")
		if err != nil {
			return nil, err
		}
		wire.Messages = append(wire.Messages, converted)
	}
	out.Body, err = json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("encode Mistral chat request: %w", err)
	}
	out.RequestType = string(llm.RequestTypeChat)
	if lo.FromPtr(wire.Stream) {
		out.Headers.Set("Accept", "text/event-stream")
	}
	return out, nil
}

func convertMessage(message openai.Message, replayReasoning bool) (chatMessage, error) {
	wire := chatMessage{Role: message.Role}
	chunks := make([]contentChunk, 0, len(message.Content.MultipleContent)+1)
	hasImage := false
	if wire.Role == "developer" {
		wire.Role = "system"
	}
	if wire.Role == "assistant" && replayReasoning && lo.FromPtr(message.ReasoningContent) != "" {
		chunks = append(chunks, contentChunk{
			Type: "thinking", Thinking: []contentChunk{{Type: "text", Text: message.ReasoningContent}},
		})
	}
	if message.Content.Content != nil {
		chunks = append(chunks, contentChunk{Type: "text", Text: message.Content.Content})
	}
	for _, part := range message.Content.MultipleContent {
		switch part.Type {
		case "text":
			chunks = append(chunks, contentChunk{Type: "text", Text: part.Text})
		case "image_url":
			hasImage = true
			image := part.ImageURL
			if image != nil {
				image = &openai.ImageURL{URL: image.URL}
			}
			chunks = append(chunks, contentChunk{Type: "image_url", ImageURL: image})
		default:
			return chatMessage{}, fmt.Errorf("%w: Mistral Vibe does not support %q message content", transformer.ErrInvalidRequest, part.Type)
		}
	}
	if hasImage || (len(chunks) > 0 && chunks[0].Type == "thinking") {
		wire.Content = chunks
	} else {
		text := strings.Join(lo.Map(chunks, func(chunk contentChunk, _ int) string {
			return lo.FromPtr(chunk.Text)
		}), "\n\n")
		if text != "" || wire.Role != "assistant" {
			wire.Content = text
		}
	}
	switch wire.Role {
	case "assistant":
		wire.ToolCalls = lo.Map(message.ToolCalls, func(call openai.ToolCall, _ int) requestToolCall {
			return requestToolCall{ID: call.ID, Type: "function", Function: call.Function}
		})
	case "tool":
		wire.Name, wire.ToolCallID = message.Name, message.ToolCallID
	case "system", "user":
	default:
		return chatMessage{}, fmt.Errorf("%w: unsupported Mistral chat role %q", transformer.ErrInvalidRequest, wire.Role)
	}
	return wire, nil
}

func (t *OutboundTransformer) TransformResponse(ctx context.Context, response *httpclient.Response) (*llm.Response, error) {
	if response == nil || response.StatusCode >= 400 || len(response.Body) == 0 {
		return t.Outbound.TransformResponse(ctx, response)
	}
	normalized := *response
	var err error
	normalized.Body, err = normalizeResponse(response.Body)
	if err != nil {
		return nil, err
	}
	return t.Outbound.TransformResponse(ctx, &normalized)
}

func (t *OutboundTransformer) TransformStream(ctx context.Context, req *httpclient.Request, stream streams.Stream[*httpclient.StreamEvent]) (streams.Stream[*llm.Response], error) {
	return t.Outbound.TransformStream(ctx, req, streams.MapErr(stream, normalizeEvent))
}

func (t *OutboundTransformer) AggregateStreamChunks(ctx context.Context, _ *httpclient.Request, chunks []*httpclient.StreamEvent) ([]byte, llm.ResponseMeta, error) {
	return openai.AggregateStreamChunks(ctx, chunks, func(ctx context.Context, event *httpclient.StreamEvent) (*openai.Response, error) {
		normalized, err := normalizeEvent(event)
		if err != nil {
			return nil, err
		}
		return openai.DefaultTransformChunk(ctx, normalized)
	})
}

func normalizeEvent(event *httpclient.StreamEvent) (*httpclient.StreamEvent, error) {
	if bytes.HasPrefix(event.Data, []byte("[DONE]")) || event.Type == "error" {
		return event, nil
	}
	normalized := *event
	var err error
	normalized.Data, err = normalizeResponse(event.Data)
	return &normalized, err
}

func normalizeResponse(body []byte) ([]byte, error) {
	var response map[string]json.RawMessage
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("decode Mistral chat response: %w", err)
	}
	var choices []map[string]json.RawMessage
	if raw := response["choices"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &choices); err != nil {
			return nil, fmt.Errorf("decode Mistral chat choices: %w", err)
		}
	}
	changed := false
	for _, choice := range choices {
		for _, field := range []string{"message", "delta"} {
			raw := choice[field]
			if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
				continue
			}
			message, err := normalizeMessage(raw)
			if err != nil {
				return nil, err
			}
			if message != nil {
				choice[field] = message
				changed = true
			}
		}
	}
	if !changed {
		return body, nil
	}
	encodedChoices, err := json.Marshal(choices)
	if err != nil {
		return nil, err
	}
	response["choices"] = encodedChoices
	return json.Marshal(response)
}

func normalizeMessage(raw json.RawMessage) (json.RawMessage, error) {
	var message map[string]json.RawMessage
	if err := json.Unmarshal(raw, &message); err != nil {
		return nil, fmt.Errorf("decode Mistral chat message: %w", err)
	}
	content := bytes.TrimSpace(message["content"])
	if !bytes.HasPrefix(content, []byte("[")) {
		return nil, nil
	}

	var chunks []contentChunk
	if err := json.Unmarshal(content, &chunks); err != nil {
		return nil, fmt.Errorf("decode Mistral chat content: %w", err)
	}

	var text, reasoning strings.Builder
	for _, chunk := range chunks {
		switch chunk.Type {
		case "text":
			text.WriteString(lo.FromPtr(chunk.Text))
		case "thinking":
			for _, part := range chunk.Thinking {
				if part.Type == "text" {
					reasoning.WriteString(lo.FromPtr(part.Text))
				}
			}
		}
	}
	if text.Len() > 0 {
		message["content"], _ = json.Marshal(text.String())
	} else {
		delete(message, "content")
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"], _ = json.Marshal(reasoning.String())
	}
	return json.Marshal(message)
}
