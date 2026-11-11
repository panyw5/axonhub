package devin

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/auth"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/transformer"
	"github.com/looplj/axonhub/llm/transformer/anthropic"
	"github.com/looplj/axonhub/llm/transformer/openai"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
)

func pbBytes(num protowire.Number, value []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, num, protowire.BytesType), value)
}
func pbInt(num protowire.Number, value uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, num, protowire.VarintType), value)
}
func pbString(num protowire.Number, value string) []byte { return pbBytes(num, []byte(value)) }
func responseBody(frames ...[]byte) []byte {
	var body []byte
	for _, f := range frames {
		body = append(body, WrapConnectEnvelope(f)...)
	}
	return append(body, WrapConnectEnvelopeWithFlag(ConnectFlagEndStream, []byte(`{}`))...)
}

func testOutbound(t *testing.T, baseURL string) *OutboundTransformer {
	t.Helper()
	outbound, err := NewOutboundTransformerWithConfig(&Config{BaseURL: baseURL, APIKeyProvider: auth.NewStaticKeyProvider("test-token")})
	require.NoError(t, err)
	return outbound
}

func TestDevinPipeline(t *testing.T) {
	for _, format := range []struct {
		name    string
		inbound transformer.Inbound
		body    string
	}{
		{"chat", openai.NewInboundTransformer(), `{"model":"swe-2","messages":[{"role":"user","content":"Hello"}]}`},
		{"messages", anthropic.NewInboundTransformer(), `{"model":"swe-2","max_tokens":100,"messages":[{"role":"user","content":"Hello"}]}`},
		{"responses", responses.NewInboundTransformer(), `{"model":"swe-2","input":"Hello"}`},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", format.name, stream), func(t *testing.T) {
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					require.Equal(t, DevinChatPath, r.URL.Path)
					require.Equal(t, "Basic devin-session-token$test-token-devin-session-token$test-token", r.Header.Get("Authorization"))
					require.Equal(t, connectContentType, r.Header.Get("Content-Type"))
					require.Empty(t, r.Header.Get("User-Agent"))
					require.Equal(t, "identity", r.Header.Get("Accept-Encoding"))
					flag, body, err := ReadConnectFrame(r.Body)
					require.NoError(t, err)
					require.Zero(t, flag)
					require.Contains(t, string(body), "swe-2-high")
					require.Contains(t, string(body), "Hello")
					usage := append(pbInt(2, 10), pbInt(3, 3)...)
					frame := append(pbString(3, "Hello from Devin"), pbBytes(7, usage)...)
					w.Header().Set("Content-Type", connectContentType)
					_, _ = w.Write(responseBody(frame))
				}))
				defer upstream.Close()
				body := strings.TrimSuffix(format.body, "}") + fmt.Sprintf(`,"stream":%v}`, stream)
				pipe := pipeline.NewFactory(httpclient.NewHttpClient()).Pipeline(format.inbound, testOutbound(t, upstream.URL))
				result, err := pipe.Process(context.Background(), &httpclient.Request{Method: "POST", URL: "http://local", Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte(body)})
				require.NoError(t, err)
				if !stream {
					require.Contains(t, string(result.Response.Body), "Hello from Devin")
					return
				}
				defer result.EventStream.Close()
				var output strings.Builder
				for result.EventStream.Next() {
					output.Write(result.EventStream.Current().Data)
				}
				require.NoError(t, result.EventStream.Err())
				require.Contains(t, output.String(), "Hello from Devin")
			})
		}
	}
}

func TestDevinWireAfterOverrides(t *testing.T) {
	outbound := testOutbound(t, "")
	req, err := outbound.TransformRequest(context.Background(), &llm.Request{Model: "swe-2", Messages: []llm.Message{{Role: "user", Content: llm.MessageContent{Content: lo.ToPtr("original")}}}})
	require.NoError(t, err)
	require.NotContains(t, string(req.Body), "test-token")
	req.Body = bytes.ReplaceAll(req.Body, []byte("original"), []byte("overridden"))
	_, err = httpclient.FinalizeAuthHeaders(req)
	require.NoError(t, err)
	wire, err := prepareWire(req)
	require.NoError(t, err)
	_, payload, err := ReadConnectFrame(bytes.NewReader(wire.Body))
	require.NoError(t, err)
	require.Contains(t, string(payload), "overridden")
	require.NotContains(t, string(payload), "original")
	require.True(t, json.Valid(req.Body))
	require.False(t, outbound.AllowPassThroughBody(context.Background(), nil, nil))
}

func TestDevinConversationConversion(t *testing.T) {
	signature := encodeSignature([]byte{0, 255, 1}, "anthropic")
	encodedID := encodeToolID("call:with/slashes")
	req := &llm.Request{Model: "swe-2", Messages: []llm.Message{
		{Role: "system", Content: llm.MessageContent{Content: lo.ToPtr("system")}},
		{Role: "developer", Content: llm.MessageContent{Content: lo.ToPtr("developer")}},
		{Role: "user", Content: llm.MessageContent{MultipleContent: []llm.MessageContentPart{{Type: "image_url", ImageURL: &llm.ImageURL{URL: "data:image/png;base64,aGVsbG8="}}}}},
		{Role: "assistant", ReasoningContent: lo.ToPtr("think"), ReasoningSignature: &signature, ToolCalls: []llm.ToolCall{{ID: encodedID, Type: "function", Function: llm.FunctionCall{Name: "lookup", Arguments: `{}`}}}},
		{Role: "tool", ToolCallID: &encodedID, Content: llm.MessageContent{Content: lo.ToPtr("result")}},
	}, Tools: []llm.Tool{{Type: "function", Function: llm.Function{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}}}}
	system, prompts, tools, err := convertMessages(req)
	require.NoError(t, err)
	require.Equal(t, "system\n\ndeveloper", system)
	require.Len(t, prompts, 3)
	require.Equal(t, "image/png", prompts[0].Images[0].MimeType)
	require.Equal(t, []byte{0, 255, 1}, prompts[1].Signature)
	require.Equal(t, "anthropic", prompts[1].SignatureType)
	require.Equal(t, "call:with/slashes", prompts[1].ToolCalls[0].ID)
	require.Equal(t, "call:with/slashes", prompts[2].ToolCallID)
	require.Len(t, tools, 1)
	for _, id := range []string{"call-legal_123", "call:with/slashes", toolIDPrefix + "literal"} {
		require.Equal(t, id, decodeToolID(encodeToolID(id)))
	}
}

func TestDevinRejectsUnsupportedInput(t *testing.T) {
	for _, body := range []string{
		`{"model":"swe-2","messages":[{"role":"user","content":[]}]}`,
		`{"model":"swe-2","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.invalid/image.png"}}]}]}`,
		`{"model":"swe-2","messages":[{"role":"user","content":[{"type":"input_audio"}]}]}`,
		`{"model":"swe-2","messages":[{"role":"tool","content":"result"}]}`,
		`{"model":"swe-2","messages":[{"role":"user","content":"hello"}],"tool_choice":"required"}`,
	} {
		var req llm.Request
		require.NoError(t, json.Unmarshal([]byte(body), &req))
		_, err := testOutbound(t, "").TransformRequest(context.Background(), &req)
		require.ErrorIs(t, err, transformer.ErrInvalidRequest)
	}
}

func TestDevinResponseToolsThinkingAndUsage(t *testing.T) {
	tool1 := append(pbString(1, "call:1"), pbString(2, "lookup")...)
	tool1 = append(tool1, pbString(3, `{"q":`)...)
	tool2 := pbString(3, `"value"}`)
	usage := append(pbInt(2, 10), pbInt(3, 5)...)
	usage = append(usage, pbInt(4, 2)...)
	usage = append(usage, pbInt(5, 7)...)
	body := responseBody(pbString(9, "think"), append(pbBytes(10, []byte("sig")), pbString(21, "anthropic")...), pbBytes(6, tool1), append(pbBytes(6, tool2), pbBytes(7, usage)...))
	outbound := testOutbound(t, "")
	req := &httpclient.Request{Body: []byte(`{"model":"swe-2"}`)}
	response, err := outbound.TransformResponse(context.Background(), &httpclient.Response{StatusCode: 200, Body: body, Request: req})
	require.NoError(t, err)
	msg := response.Choices[0].Message
	require.Equal(t, "think", *msg.ReasoningContent)
	require.Equal(t, `{"q":"value"}`, msg.ToolCalls[0].Function.Arguments)
	require.Equal(t, "call:1", decodeToolID(msg.ToolCalls[0].ID))
	require.Equal(t, int64(19), response.Usage.PromptTokens)
	require.Equal(t, int64(24), response.Usage.TotalTokens)
	sig, typ := decodeSignature(*msg.ReasoningSignature)
	require.Equal(t, "sig", string(sig))
	require.Equal(t, "anthropic", typ)
	decoder := newDecoder(context.Background(), io.NopCloser(bytes.NewReader(body)))
	stream, err := outbound.TransformStream(context.Background(), req, decoder)
	require.NoError(t, err)
	var chunks []*httpclient.StreamEvent
	var seen []string
	for stream.Next() {
		r := stream.Current()
		if r.Object == "[DONE]" {
			seen = append(seen, "done")
			continue
		}
		for _, choice := range r.Choices {
			if choice.Delta.ReasoningContent != nil {
				seen = append(seen, "thinking")
			}
			if choice.Delta.ReasoningSignature != nil {
				seen = append(seen, "signature")
			}
			if len(choice.Delta.ToolCalls) > 0 {
				seen = append(seen, "tool")
			}
		}
	}
	require.NoError(t, stream.Err())
	require.Equal(t, []string{"thinking", "signature", "tool", "tool", "done"}, seen)
	decoder = newDecoder(context.Background(), io.NopCloser(bytes.NewReader(body)))
	for decoder.Next() {
		chunks = append(chunks, decoder.Current())
	}
	_, meta, err := outbound.AggregateStreamChunks(context.Background(), req, chunks)
	require.NoError(t, err)
	require.True(t, meta.Completed)
	require.Equal(t, response.Usage, meta.Usage)
}

func TestDevinFrameBoundaries(t *testing.T) {
	text := []byte("\u4f60\u597d")
	body := responseBody(pbBytes(3, text[:1]), pbBytes(3, text[1:4]), pbBytes(3, text[4:]))
	response, err := testOutbound(t, "").TransformResponse(context.Background(), &httpclient.Response{StatusCode: 200, Body: body})
	require.NoError(t, err)
	require.Equal(t, string(text), *response.Choices[0].Message.Content.Content)
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	_, err = zw.Write(pbString(3, "compressed"))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	body = append(WrapConnectEnvelopeWithFlag(ConnectFlagCompressed, compressed.Bytes()), WrapConnectEnvelopeWithFlag(ConnectFlagEndStream, []byte(`{}`))...)
	response, err = testOutbound(t, "").TransformResponse(context.Background(), &httpclient.Response{StatusCode: 200, Body: body})
	require.NoError(t, err)
	require.Equal(t, "compressed", *response.Choices[0].Message.Content.Content)
	args := []byte("{\"q\":\"\u4f60\u597d\"}")
	first := append(pbString(1, "call-one"), pbString(2, "lookup")...)
	first = append(first, pbBytes(3, args[:7])...)
	body = responseBody(pbBytes(6, first), pbBytes(6, pbBytes(3, args[7:])))
	response, err = testOutbound(t, "").TransformResponse(context.Background(), &httpclient.Response{StatusCode: 200, Body: body})
	require.NoError(t, err)
	require.Equal(t, string(args), response.Choices[0].Message.ToolCalls[0].Function.Arguments)
}

func TestDevinStreamIdentityAndBinarySignature(t *testing.T) {
	body := responseBody(append(pbString(1, "upstream-id"), pbString(3, "hello")...), pbString(1, "later-id"))
	decoder := newDecoder(context.Background(), io.NopCloser(bytes.NewReader(body)))
	stream, err := testOutbound(t, "").TransformStream(context.Background(), nil, decoder)
	require.NoError(t, err)
	for stream.Next() {
		if stream.Current().Object != "[DONE]" {
			require.Equal(t, "upstream-id", stream.Current().ID)
		}
	}
	require.NoError(t, stream.Err())
	for _, data := range [][]byte{{0, 255, 1}, []byte(signaturePrefix + "literal")} {
		restored, typ := decodeSignature(encodeSignature(data, ""))
		require.Equal(t, data, restored)
		require.Empty(t, typ)
	}
}

func TestDevinRejectsBrokenStreams(t *testing.T) {
	for _, body := range [][]byte{
		WrapConnectEnvelope(pbString(3, "partial")),
		{0, 0, 0},
		WrapConnectEnvelopeWithFlag(ConnectFlagEndStream, []byte(`not-json`)),
		responseBody([]byte{0xff}),
	} {
		_, err := testOutbound(t, "").TransformResponse(context.Background(), &httpclient.Response{StatusCode: 200, Body: body})
		require.Error(t, err)
	}
	decoder := newDecoder(context.Background(), io.NopCloser(bytes.NewReader(WrapConnectEnvelope(pbString(3, "partial")))))
	for decoder.Next() {
	}
	require.ErrorIs(t, decoder.Err(), llm.ErrStreamIncomplete)
	tooLarge := make([]byte, 5)
	binary.BigEndian.PutUint32(tooLarge[1:], maxConnectFrameSize+1)
	_, _, err := ReadConnectFrame(bytes.NewReader(tooLarge))
	require.ErrorContains(t, err, "maximum")
	for _, code := range []string{"resource_exhausted", "unauthenticated", "unavailable"} {
		body := WrapConnectEnvelopeWithFlag(ConnectFlagEndStream, []byte(fmt.Sprintf(`{"error":{"code":%q,"message":"failed"}}`, code)))
		_, err := testOutbound(t, "").TransformResponse(context.Background(), &httpclient.Response{StatusCode: 200, Body: body})
		var upstreamErr *llm.ResponseError
		require.True(t, errors.As(err, &upstreamErr))
		if code == "resource_exhausted" {
			require.Equal(t, 429, upstreamErr.StatusCode)
		}
	}
}

func TestDevinModels(t *testing.T) {
	require.NotEmpty(t, Models())
	require.Equal(t, lookupModel("swe-2").MaxCompletionTokens, lookupModel("devin/swe-2-high").MaxCompletionTokens)
	for _, tc := range []struct{ model, effort, want string }{
		{"swe-2", "", "swe-2-high"}, {"devin/swe-2", "max", "swe-2-max"},
		{"swe-2-medium", "max", "swe-2-medium"}, {"swe-1-6", "", "swe-1-6"},
		{"claude-haiku-4-5", "", "MODEL_PRIVATE_11"},
	} {
		require.Equal(t, tc.want, ResolveDevinChatModelUID(tc.model, tc.effort, 0))
	}
}

func TestDevinUserStatus(t *testing.T) {
	plan := append(pbInt(14, 0), pbInt(15, 85)...)
	plan = append(plan, pbInt(17, 2000000000)...)
	status, err := ParseUserStatus(pbBytes(1, pbBytes(13, plan)))
	require.NoError(t, err)
	require.Zero(t, *status.DailyRemaining)
	require.Equal(t, int64(85), *status.WeeklyRemaining)
	require.Equal(t, int64(2000000000), status.DailyReset.Unix())
	for _, invalid := range [][]byte{nil, {0xff}, pbBytes(1, pbBytes(13, pbInt(14, 101))), pbBytes(1, []byte{0xff})} {
		_, err := ParseUserStatus(invalid)
		require.Error(t, err)
	}
}
