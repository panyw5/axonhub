package commandcode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
		WorkingDir:     "/Users/dev/axonhub",
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
	require.Equal(t, Version, req.Headers.Get("X-Command-Code-Version"))
	require.Equal(t, "axonhub", req.Headers.Get("X-Project-Slug"))
	require.Equal(t, "cli", req.Headers.Get("User-Agent"))
	require.Equal(t, "*/*", req.Headers.Get("Accept"))
	require.Equal(t, "production", req.Headers.Get("X-Cli-Environment"))
	require.Equal(t, "true", req.Headers.Get("X-Taste-Learning"))
	require.Empty(t, req.Headers.Get("X-Co-Flag"))
	require.Regexp(t, `^[0-9a-f-]{36}$`, req.Headers.Get("X-Session-Id"))
	require.Regexp(t, `^00-[0-9a-f]{32}-[0-9a-f]{16}-01$`, req.Headers.Get("traceparent"))
	require.Equal(t, "application/jsonl", req.Metadata[httpclient.MetadataStreamDecoderContentType])

	var body requestEnvelope
	require.NoError(t, json.Unmarshal(req.Body, &body))
	require.Equal(t, "standard", body.PermissionMode)
	require.Equal(t, req.Headers.Get("X-Session-Id"), body.ThreadID)
	require.Equal(t, "/Users/dev/axonhub", body.Config.WorkingDir)
	require.Equal(t, nodePlatform(), body.Config.Environment)
	require.Equal(t, "gpt-5.6-sol", body.Params.Model)
	require.Equal(t, "Be concise.", body.Params.System)
	require.Equal(t, defaultMaxOutputTokens, body.Params.MaxTokens)
	require.Equal(t, 0.7, *body.Params.Temperature)
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
	require.Len(t, responses, 3)
	require.Equal(t, "Hello", *responses[0].Choices[0].Delta.Content.Content)
	require.Equal(t, "stop", *responses[1].Choices[0].FinishReason)
	require.Equal(t, llm.DoneResponse, responses[2])
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
	require.Len(t, responses, 5)
	require.Equal(t, "Think", *responses[0].Choices[0].Delta.ReasoningContent)
	require.Equal(t, "Hello", *responses[1].Choices[0].Delta.Content.Content)
	require.Equal(t, "read", responses[2].Choices[0].Delta.ToolCalls[0].Function.Name)
	require.Equal(t, `{"path":"a"}`, responses[2].Choices[0].Delta.ToolCalls[0].Function.Arguments)
	require.Equal(t, "tool_calls", *responses[3].Choices[0].FinishReason)
	require.Equal(t, llm.DoneResponse, responses[4])
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
	var providerErr *llm.ResponseError
	require.ErrorAs(t, err, &providerErr)
	require.Equal(t, "quota exhausted", providerErr.Detail.Message)

	transformed := transformer.TransformError(context.Background(), &httpclient.Error{StatusCode: http.StatusUnauthorized, Body: []byte("invalid key")})
	require.Equal(t, http.StatusUnauthorized, transformed.StatusCode)
	require.Equal(t, "invalid key", transformed.Detail.Message)
}

func TestCommandCodeStreamCompletion(t *testing.T) {
	outbound, err := NewOutboundTransformerWithConfig(&Config{APIKeyProvider: auth.NewStaticKeyProvider("test-key")})
	require.NoError(t, err)

	for _, tt := range []struct {
		name      string
		trailer   string
		completed bool
	}{
		{name: "native finish", trailer: `{"type":"finish","finishReason":"stop"}`, completed: true},
		{name: "SSE finish", trailer: "event: message\nid: 2\n: keepalive\ndata: {\"type\":\"finish\"}\n\n", completed: true},
		{name: "EOF without finish"},
		{name: "DONE without finish", trailer: "data: [DONE]"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := `{"type":"text-delta","text":"Hello"}` + "\n" + tt.trailer
			raw := newJSONLDecoder(context.Background(), io.NopCloser(strings.NewReader(body)))
			stream, err := outbound.TransformStream(context.Background(), nil, raw)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, stream.Close()) })
			require.True(t, stream.Next())
			require.Equal(t, "Hello", *stream.Current().Choices[0].Delta.Content.Content)
			if tt.completed {
				require.True(t, stream.Next())
				require.Equal(t, "stop", *stream.Current().Choices[0].FinishReason)
				require.True(t, stream.Next())
				require.Equal(t, llm.DoneResponse, stream.Current())
				require.False(t, stream.Next())
				require.NoError(t, stream.Err())
				_, err = outbound.TransformResponse(context.Background(), &httpclient.Response{Body: []byte(body)})
				require.NoError(t, err)
			} else {
				require.False(t, stream.Next())
				require.ErrorIs(t, stream.Err(), llm.ErrStreamIncomplete)
				_, err = outbound.TransformResponse(context.Background(), &httpclient.Response{Body: []byte(body)})
				require.ErrorIs(t, err, llm.ErrStreamIncomplete)
			}

			var chunks []*httpclient.StreamEvent
			for _, line := range strings.Split(body, "\n") {
				chunks = append(chunks, &httpclient.StreamEvent{Data: []byte(line)})
			}
			_, meta, err := outbound.AggregateStreamChunks(context.Background(), nil, chunks)
			require.NoError(t, err)
			require.Equal(t, tt.completed, meta.Completed)
		})
	}
}

func TestTransformStreamPauseTurnContinuation(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/alpha/generate" {
			return
		}
		calls++
		if calls == 1 {
			_, _ = w.Write([]byte("{\"type\":\"text-delta\",\"text\":\"Hello\"}\n" +
				"{\"type\":\"finish\",\"finishReason\":\"stop\",\"rawFinishReason\":\"pause_turn\"}\n"))
			return
		}
		_, _ = w.Write([]byte("{\"type\":\"text-delta\",\"text\":\" world\"}\n" +
			"{\"type\":\"finish\",\"finishReason\":\"stop\",\"rawFinishReason\":\"end_turn\"," +
			"\"totalUsage\":{\"inputTokens\":3,\"outputTokens\":2}}\n"))
	}))
	defer server.Close()

	outbound, err := NewOutboundTransformerWithConfig(&Config{
		BaseURL:        server.URL,
		APIKeyProvider: auth.NewStaticKeyProvider("test-key"),
		HTTPClient:     httpclient.NewHttpClient(),
	})
	require.NoError(t, err)
	req, err := outbound.TransformRequest(context.Background(), &llm.Request{
		Model:    "gpt-5.6-sol",
		Messages: []llm.Message{{Role: "user", Content: llm.MessageContent{Content: lo.ToPtr("Hello")}}},
	})
	require.NoError(t, err)

	raw, err := httpclient.NewHttpClient().DoStream(context.Background(), req)
	require.NoError(t, err)
	stream, err := outbound.TransformStream(context.Background(), req, raw)
	require.NoError(t, err)

	var texts []string
	var finish *llm.Response
	for stream.Next() {
		cur := stream.Current()
		if cur == nil || len(cur.Choices) == 0 {
			continue
		}
		if cur.Choices[0].Delta != nil && cur.Choices[0].Delta.Content.Content != nil {
			texts = append(texts, *cur.Choices[0].Delta.Content.Content)
		}
		if cur.Choices[0].FinishReason != nil {
			finish = cur
		}
	}
	require.NoError(t, stream.Err())
	require.Equal(t, 2, calls)
	require.Equal(t, []string{"Hello", " world"}, texts)
	require.NotNil(t, finish)
	require.Equal(t, "stop", *finish.Choices[0].FinishReason)
	require.Equal(t, int64(5), finish.Usage.TotalTokens)
}

func TestTransformResponsePauseTurnContinuation(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/alpha/generate" {
			return
		}
		calls++
		if calls == 1 {
			_, _ = w.Write([]byte("{\"type\":\"text-delta\",\"text\":\"Hello\"}\n" +
				"{\"type\":\"finish\",\"finishReason\":\"stop\",\"rawFinishReason\":\"pause_turn\"}\n"))
			return
		}
		_, _ = w.Write([]byte("{\"type\":\"text-delta\",\"text\":\" world\"}\n" +
			"{\"type\":\"finish\",\"finishReason\":\"stop\",\"rawFinishReason\":\"end_turn\"}\n"))
	}))
	defer server.Close()

	outbound, err := NewOutboundTransformerWithConfig(&Config{
		BaseURL:        server.URL,
		APIKeyProvider: auth.NewStaticKeyProvider("test-key"),
		HTTPClient:     httpclient.NewHttpClient(),
	})
	require.NoError(t, err)
	req, err := outbound.TransformRequest(context.Background(), &llm.Request{
		Model:    "gpt-5.6-sol",
		Messages: []llm.Message{{Role: "user", Content: llm.MessageContent{Content: lo.ToPtr("Hello")}}},
	})
	require.NoError(t, err)

	resp, err := httpclient.NewHttpClient().Do(context.Background(), req)
	require.NoError(t, err)
	result, err := outbound.TransformResponse(context.Background(), resp)
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	require.Equal(t, "Hello world", *result.Choices[0].Message.Content.Content)
	require.Equal(t, "stop", *result.Choices[0].FinishReason)
}

func TestTransformStreamTruncatedRestart(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/alpha/generate" {
			return
		}
		calls++
		if calls == 1 {
			_, _ = w.Write([]byte("{\"type\":\"text-delta\",\"text\":\"Hello\"}\n"))
			return
		}
		_, _ = w.Write([]byte("{\"type\":\"text-delta\",\"text\":\" world\"}\n" +
			"{\"type\":\"finish\",\"finishReason\":\"stop\",\"rawFinishReason\":\"end_turn\"}\n"))
	}))
	defer server.Close()

	outbound, err := NewOutboundTransformerWithConfig(&Config{
		BaseURL:        server.URL,
		APIKeyProvider: auth.NewStaticKeyProvider("test-key"),
		HTTPClient:     httpclient.NewHttpClient(),
	})
	require.NoError(t, err)
	req, err := outbound.TransformRequest(context.Background(), &llm.Request{
		Model:    "gpt-5.6-sol",
		Messages: []llm.Message{{Role: "user", Content: llm.MessageContent{Content: lo.ToPtr("Hello")}}},
	})
	require.NoError(t, err)

	raw, err := httpclient.NewHttpClient().DoStream(context.Background(), req)
	require.NoError(t, err)
	stream, err := outbound.TransformStream(context.Background(), req, raw)
	require.NoError(t, err)

	var texts []string
	var finish *llm.Response
	for stream.Next() {
		cur := stream.Current()
		if cur == nil || len(cur.Choices) == 0 {
			continue
		}
		if cur.Choices[0].Delta != nil && cur.Choices[0].Delta.Content.Content != nil {
			texts = append(texts, *cur.Choices[0].Delta.Content.Content)
		}
		if cur.Choices[0].FinishReason != nil {
			finish = cur
		}
	}
	require.NoError(t, stream.Err())
	require.Equal(t, 2, calls)
	require.Equal(t, []string{"Hello", " world"}, texts)
	require.NotNil(t, finish)
	require.Equal(t, "stop", *finish.Choices[0].FinishReason)
}

func TestTransformStreamTruncatedRestartBudget(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/alpha/generate" {
			return
		}
		calls++
		_, _ = w.Write([]byte("{\"type\":\"text-delta\",\"text\":\"Hello\"}\n"))
	}))
	defer server.Close()

	outbound, err := NewOutboundTransformerWithConfig(&Config{
		BaseURL:        server.URL,
		APIKeyProvider: auth.NewStaticKeyProvider("test-key"),
		HTTPClient:     httpclient.NewHttpClient(),
	})
	require.NoError(t, err)
	req, err := outbound.TransformRequest(context.Background(), &llm.Request{
		Model:    "gpt-5.6-sol",
		Messages: []llm.Message{{Role: "user", Content: llm.MessageContent{Content: lo.ToPtr("Hello")}}},
	})
	require.NoError(t, err)

	raw, err := httpclient.NewHttpClient().DoStream(context.Background(), req)
	require.NoError(t, err)
	stream, err := outbound.TransformStream(context.Background(), req, raw)
	require.NoError(t, err)
	for stream.Next() {
	}
	require.ErrorIs(t, stream.Err(), llm.ErrStreamIncomplete)
	require.Equal(t, 1+maxRestartsAfterContent, calls)
}

func TestTransformResponseTruncatedRestart(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/alpha/generate" {
			return
		}
		calls++
		if calls == 1 {
			_, _ = w.Write([]byte("{\"type\":\"text-delta\",\"text\":\"partial\"}\n"))
			return
		}
		_, _ = w.Write([]byte("{\"type\":\"text-delta\",\"text\":\"Hello world\"}\n" +
			"{\"type\":\"finish\",\"finishReason\":\"stop\",\"rawFinishReason\":\"end_turn\"}\n"))
	}))
	defer server.Close()

	outbound, err := NewOutboundTransformerWithConfig(&Config{
		BaseURL:        server.URL,
		APIKeyProvider: auth.NewStaticKeyProvider("test-key"),
		HTTPClient:     httpclient.NewHttpClient(),
	})
	require.NoError(t, err)
	req, err := outbound.TransformRequest(context.Background(), &llm.Request{
		Model:    "gpt-5.6-sol",
		Messages: []llm.Message{{Role: "user", Content: llm.MessageContent{Content: lo.ToPtr("Hello")}}},
	})
	require.NoError(t, err)

	resp, err := httpclient.NewHttpClient().Do(context.Background(), req)
	require.NoError(t, err)
	result, err := outbound.TransformResponse(context.Background(), resp)
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	require.Equal(t, "Hello world", *result.Choices[0].Message.Content.Content)
}

func TestFingerprintRecordedOnce(t *testing.T) {
	var fingerprintCalls int
	var fingerprintBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/alpha/fingerprint/record" {
			fingerprintCalls++
			fingerprintBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	outbound, err := NewOutboundTransformerWithConfig(&Config{
		BaseURL:        server.URL,
		APIKeyProvider: auth.NewStaticKeyProvider("test-key"),
		HTTPClient:     httpclient.NewHttpClient(),
	})
	require.NoError(t, err)
	for range 2 {
		_, err = outbound.TransformRequest(context.Background(), &llm.Request{
			Model:    "gpt-5.6-sol",
			Messages: []llm.Message{{Role: "user", Content: llm.MessageContent{Content: lo.ToPtr("Hi")}}},
		})
		require.NoError(t, err)
	}
	require.Eventually(t, func() bool { return fingerprintCalls == 1 }, 5*time.Second, 10*time.Millisecond)

	var payload struct {
		Thumbmark  string `json:"thumbmark"`
		Components struct {
			Runtime          string `json:"runtime"`
			CollectorVersion int    `json:"collectorVersion"`
			Platform         string `json:"platform"`
			Arch             string `json:"arch"`
		} `json:"components"`
	}
	require.NoError(t, json.Unmarshal(fingerprintBody, &payload))
	require.Regexp(t, `^[0-9a-f]{64}$`, payload.Thumbmark)
	require.Equal(t, "cli", payload.Components.Runtime)
	require.Equal(t, 1, payload.Components.CollectorVersion)
	require.Equal(t, nodePlatform(), payload.Components.Platform)
	require.Equal(t, nodeArch(), payload.Components.Arch)
}

func TestCommandCodeStreamPreservesProviderError(t *testing.T) {
	outbound, err := NewOutboundTransformerWithConfig(&Config{APIKeyProvider: auth.NewStaticKeyProvider("test-key")})
	require.NoError(t, err)
	for _, tt := range []struct {
		name    string
		event   string
		status  int
		message string
	}{
		{name: "structured", event: `{"type":"error","error":{"message":"Overloaded","statusCode":503}}`, status: 503, message: "Overloaded"},
		{name: "embedded", event: `{"type":"error","error":"403 {\"error\":{\"type\":\"upgrade_required\",\"message\":\"Upgrade required\"}}"}`, status: 403, message: "Upgrade required"},
		{name: "abort", event: `{"type":"abort"}`, status: 502, message: "Command Code aborted the stream"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw := streams.SliceStream([]*httpclient.StreamEvent{{Data: []byte(tt.event)}})
			stream, err := outbound.TransformStream(context.Background(), nil, raw)
			require.NoError(t, err)
			require.False(t, stream.Next())
			var providerErr *llm.ResponseError
			require.ErrorAs(t, stream.Err(), &providerErr)
			require.Equal(t, tt.status, providerErr.StatusCode)
			require.Equal(t, tt.message, providerErr.Detail.Message)
		})
	}
}
