// 职责: 验证 Mistral Vibe 的推理历史、工具调用及流式响应转换。
// 关联文件:
//
//	llm/transformer/mistral/outbound.go
package mistral

import (
	"encoding/json"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/auth"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer/openai"
)

func TestTransformRequestReasoning(t *testing.T) {
	outbound, err := NewOutboundTransformer("", "", auth.NewStaticKeyProvider("test-key"))
	require.NoError(t, err)

	for _, test := range []struct {
		name, model, effort, wireEffort, assistantContent string
	}{
		{"mistral 422 regression", "deepseek-v4-1-flash-1017", "", "", `"answer"`},
		{"reasoning enabled", defaultModel, "high", "high", `[{"type":"thinking","thinking":[{"type":"text","text":"history"}]},{"type":"text","text":"answer"}]`},
		{"Vibe default reasoning", defaultModel, "", "high", `[{"type":"thinking","thinking":[{"type":"text","text":"history"}]},{"type":"text","text":"answer"}]`},
		{"reasoning explicitly disabled", defaultModel, "none", "none", `"answer"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := &llm.Request{
				Model: test.model, ReasoningEffort: test.effort,
				Stream: lo.ToPtr(true), StreamOptions: &llm.StreamOptions{IncludeUsage: true},
				MaxCompletionTokens: lo.ToPtr(int64(128)),
				Messages: []llm.Message{
					{Role: "assistant", Content: llm.MessageContent{Content: lo.ToPtr("answer")}, ReasoningContent: lo.ToPtr("history")},
					{Role: "assistant", ReasoningContent: lo.ToPtr(""), ToolCalls: []llm.ToolCall{
						{ID: "abc123xyz", Type: "function", Function: llm.FunctionCall{Name: "lookup", Arguments: "{}"}},
					}},
					{Role: "tool", ToolCallID: lo.ToPtr("abc123xyz"), Content: llm.MessageContent{Content: lo.ToPtr("result")}},
				},
			}
			out, err := outbound.TransformRequest(t.Context(), request)
			require.NoError(t, err)
			require.Equal(t, DefaultBaseURL+"/chat/completions", out.URL)
			require.Equal(t, "text/event-stream", out.Headers.Get("Accept"))
			require.Equal(t, "test-key", out.Auth.APIKey)
			require.False(t, outbound.AllowPassThroughBody(t.Context(), request, out))
			require.NotContains(t, string(out.Body), "reasoning_content")
			require.NotContains(t, string(out.Body), "max_completion_tokens")
			require.NotContains(t, string(out.Body), "stream_options")

			var body struct {
				Messages []struct {
					Content    json.RawMessage `json:"content"`
					ToolCallID string          `json:"tool_call_id"`
					ToolCalls  json.RawMessage `json:"tool_calls"`
				} `json:"messages"`
				MaxTokens       int    `json:"max_tokens"`
				ReasoningEffort string `json:"reasoning_effort"`
			}
			require.NoError(t, json.Unmarshal(out.Body, &body))
			require.Equal(t, 128, body.MaxTokens)
			require.Equal(t, test.wireEffort, body.ReasoningEffort)
			require.JSONEq(t, test.assistantContent, string(body.Messages[0].Content))
			require.JSONEq(t, "null", string(body.Messages[1].Content))
			require.JSONEq(t, `[{"id":"abc123xyz","type":"function","function":{"name":"lookup","arguments":"{}"}}]`, string(body.Messages[1].ToolCalls))
			require.Equal(t, "abc123xyz", body.Messages[2].ToolCallID)
		})
	}
}

func TestTransformResponseAndStreamReasoning(t *testing.T) {
	outbound, err := NewOutboundTransformer("", "", auth.NewStaticKeyProvider("test-key"))
	require.NoError(t, err)

	response, err := outbound.TransformResponse(t.Context(), &httpclient.Response{
		StatusCode: 200,
		Body:       []byte(`{"choices":[{"message":{"role":"assistant","content":[{"type":"thinking","thinking":[{"type":"text","text":"plan"}]},{"type":"text","text":"done"}]},"finish_reason":"stop"}]}`),
	})
	require.NoError(t, err)
	require.Equal(t, "plan", lo.FromPtr(response.Choices[0].Message.ReasoningContent))
	require.Equal(t, "done", lo.FromPtr(response.Choices[0].Message.Content.Content))

	events := []*httpclient.StreamEvent{
		{Data: []byte(`{"id":"test","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":[{"type":"thinking","thinking":[{"type":"text","text":"plan"}]},{"type":"text","text":"done"}]}}]}`)},
		{Data: []byte(`{"id":"test","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"!"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`)},
		{Data: []byte("[DONE]")},
	}
	stream, err := outbound.TransformStream(t.Context(), nil, streams.SliceStream(events))
	require.NoError(t, err)
	defer stream.Close()
	require.True(t, stream.Next())
	require.Equal(t, "plan", lo.FromPtr(stream.Current().Choices[0].Delta.ReasoningContent))
	require.Equal(t, "done", lo.FromPtr(stream.Current().Choices[0].Delta.Content.Content))
	require.True(t, stream.Next())
	require.Equal(t, "!", lo.FromPtr(stream.Current().Choices[0].Delta.Content.Content))
	require.True(t, stream.Next())
	require.Same(t, llm.DoneResponse, stream.Current())
	require.False(t, stream.Next())
	require.NoError(t, stream.Err())

	aggregated, _, err := outbound.AggregateStreamChunks(t.Context(), nil, events)
	require.NoError(t, err)
	var completion openai.Response
	require.NoError(t, json.Unmarshal(aggregated, &completion))
	require.Equal(t, "plan", lo.FromPtr(completion.Choices[0].Message.ReasoningContent))
	require.Equal(t, "done!", lo.FromPtr(completion.Choices[0].Message.Content.Content))
}
