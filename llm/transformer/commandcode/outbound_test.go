package commandcode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/auth"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
)

func TestTransformRequest(t *testing.T) {
	transformer, err := NewOutboundTransformerWithConfig(&Config{
		BaseURL:        "https://commandcode.test/",
		APIKeyProvider: auth.NewStaticKeyProvider("test-key"),
	})
	require.NoError(t, err)

	system := "Be concise."
	user := "Hello"
	arguments := `{"path":"a.txt"}`
	toolCallID := "call-1"
	toolOutput := "contents"
	reasoning := "Need to inspect the file first."
	maxTokens := int64(100_000)
	temperature := 0.7
	req, err := transformer.TransformRequest(context.Background(), &llm.Request{
		Model: "gpt-5.6-sol",
		Messages: []llm.Message{
			{Role: "system", Content: llm.MessageContent{Content: &system}},
			{Role: "user", Content: llm.MessageContent{Content: &user}},
			{Role: "assistant", ReasoningContent: &reasoning, ToolCalls: []llm.ToolCall{{ID: toolCallID, Function: llm.FunctionCall{Name: "read", Arguments: arguments}}}},
			{Role: "tool", ToolCallID: &toolCallID, Content: llm.MessageContent{Content: &toolOutput}},
		},
		Tools: []llm.Tool{{Type: "function", Function: llm.Function{
			Name: "read", Description: "Read a file", Parameters: json.RawMessage(`{"type":"object"}`),
		}}},
		MaxTokens: &maxTokens, Temperature: &temperature, ReasoningEffort: "high",
	})
	require.NoError(t, err)
	require.Equal(t, "https://commandcode.test/alpha/generate", req.URL)
	require.Equal(t, "Bearer test-key", req.Headers.Get("Authorization"))
	require.Equal(t, commandCodeVersion, req.Headers.Get("X-Command-Code-Version"))
	require.Equal(t, "axonhub", req.Headers.Get("X-Project-Slug"))
	require.Equal(t, "application/jsonl", req.Metadata[httpclient.MetadataStreamDecoderContentType])

	var body requestEnvelope
	require.NoError(t, json.Unmarshal(req.Body, &body))
	require.Equal(t, "gpt-5.6-sol", body.Params.Model)
	require.Equal(t, "Be concise.", body.Params.System)
	require.Equal(t, defaultMaxOutputTokens, body.Params.MaxTokens)
	require.Equal(t, 0.7, body.Params.Temperature)
	require.Equal(t, "high", body.Params.ReasoningEffort)
	require.True(t, body.Params.Stream)
	require.Len(t, body.Params.Messages, 3)
	require.Equal(t, map[string]any{"type": "reasoning", "text": reasoning}, body.Params.Messages[1].Content[0])
	require.Len(t, body.Params.Tools, 1)
}

func TestCommandCodeSSEHeaderUsesJSONLDecoder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("{\"type\":\"start\"}\n" +
			"{\"type\":\"text-delta\",\"text\":\"Hello\"}\n" +
			"{\"type\":\"finish\",\"finishReason\":\"stop\"}\n"))
	}))
	defer server.Close()

	outbound, err := NewOutboundTransformerWithConfig(&Config{
		BaseURL: server.URL, APIKeyProvider: auth.NewStaticKeyProvider("test-key"),
	})
	require.NoError(t, err)
	req, err := outbound.TransformRequest(context.Background(), &llm.Request{
		Model:    "deepseek/deepseek-v4-flash",
		Messages: []llm.Message{{Role: "user", Content: llm.MessageContent{Content: lo.ToPtr("Hello")}}},
	})
	require.NoError(t, err)

	raw, err := httpclient.NewHttpClient().DoStream(context.Background(), req)
	require.NoError(t, err)
	stream, err := outbound.TransformStream(context.Background(), req, raw)
	require.NoError(t, err)

	var responses []*llm.Response
	for stream.Next() {
		if stream.Current() != nil {
			responses = append(responses, stream.Current())
		}
	}
	require.NoError(t, stream.Err())
	require.Len(t, responses, 2)
	require.Equal(t, "Hello", *responses[0].Choices[0].Delta.Content.Content)
	require.Equal(t, "stop", *responses[1].Choices[0].FinishReason)
}

func TestTransformStream(t *testing.T) {
	transformer, err := NewOutboundTransformerWithConfig(&Config{APIKeyProvider: auth.NewStaticKeyProvider("test-key")})
	require.NoError(t, err)

	request, err := transformer.TransformRequest(context.Background(), &llm.Request{
		Model:    "gpt-5.6-sol",
		Messages: []llm.Message{{Role: "user", Content: llm.MessageContent{Content: lo.ToPtr("Hello")}}},
	})
	require.NoError(t, err)

	raw := streams.SliceStream([]*httpclient.StreamEvent{
		{Data: []byte(`{"type":"start"}`)},
		{Data: []byte(`{"type":"reasoning-start"}`)},
		{Data: []byte(`{"type":"reasoning-delta","text":"Think"}`)},
		{Data: []byte(`{"type":"reasoning-end"}`)},
		{Data: []byte(`{"type":"text-delta","text":"Hello"}`)},
		{Data: []byte(`{"type":"provider-metadata","providerMetadata":{}}`)},
		{Data: []byte(`{"type":"tool-call","toolCallId":"call-1","toolName":"read","input":{"path":"a"}}`)},
		{Data: []byte(`{"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":10,"outputTokens":3,"inputTokenDetails":{"cacheReadTokens":7,"cacheWriteTokens":2},"cacheWriteTokens1h":1}}`)},
	})
	converted, err := transformer.TransformStream(context.Background(), request, raw)
	require.NoError(t, err)

	var responses []*llm.Response
	for converted.Next() {
		responses = append(responses, converted.Current())
	}
	require.NoError(t, converted.Err())
	require.Len(t, responses, 4)
	require.Equal(t, "Think", *responses[0].Choices[0].Delta.ReasoningContent)
	require.Equal(t, "Hello", *responses[1].Choices[0].Delta.Content.Content)
	require.Equal(t, "read", responses[2].Choices[0].Delta.ToolCalls[0].Function.Name)
	require.Equal(t, `{"path":"a"}`, responses[2].Choices[0].Delta.ToolCalls[0].Function.Arguments)
	require.Equal(t, "tool_calls", *responses[3].Choices[0].FinishReason)
	require.Equal(t, int64(13), responses[3].Usage.TotalTokens)
	require.Equal(t, int64(7), responses[3].Usage.PromptTokensDetails.CachedTokens)
	require.Equal(t, int64(2), responses[3].Usage.PromptTokensDetails.WriteCachedTokens)
	require.Equal(t, int64(1), responses[3].Usage.PromptTokensDetails.WriteCached1HourTokens)
}

func TestTransformResponseAndError(t *testing.T) {
	transformer, err := NewOutboundTransformerWithConfig(&Config{APIKeyProvider: auth.NewStaticKeyProvider("test-key")})
	require.NoError(t, err)

	httpResponse := &httpclient.Response{
		Body: []byte("data: {\"type\":\"text-delta\",\"text\":\"Hello\"}\n" +
			"{\"type\":\"finish\",\"finishReason\":\"stop\",\"totalUsage\":{\"inputTokens\":2,\"outputTokens\":1}}\n"),
	}
	response, err := transformer.TransformResponse(context.Background(), httpResponse)
	require.NoError(t, err)
	require.Equal(t, "Hello", *response.Choices[0].Message.Content.Content)
	require.Equal(t, int64(3), response.Usage.TotalTokens)
	require.True(t, json.Valid(httpResponse.Body))

	_, err = transformer.TransformResponse(context.Background(), &httpclient.Response{
		Body: []byte(`{"type":"error","error":{"message":"quota exhausted"}}`),
	})
	require.EqualError(t, err, "Command Code stream error: quota exhausted")

	transformed := transformer.TransformError(context.Background(), &httpclient.Error{StatusCode: http.StatusUnauthorized, Body: []byte("invalid key")})
	require.Equal(t, http.StatusUnauthorized, transformed.StatusCode)
	require.Equal(t, "invalid key", transformed.Detail.Message)
}
