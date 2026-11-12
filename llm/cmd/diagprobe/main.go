// diagprobe: diagnostic tool for the encrypted-content corruption investigation.
// It sends a streaming request through the REAL codex outbound transformer +
// httpclient to the Aether upstream, prints every raw SSE event's
// reasoning-relevant fields, then runs the captured events through the
// outbound → inbound Responses transformers and prints client-visible items.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/samber/lo"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/auth"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/oauth"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer/openai/codex"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
)

func main() {
	baseURL := "http://localhost:8084"
	apiKey := os.Getenv("AETHER_KEY")
	if apiKey == "" {
		fmt.Println("AETHER_KEY env required")
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	tokens := oauth.NewAPIKeyTokenProvider(func(ctx context.Context) string { return apiKey })
	codexOutbound, err := codex.NewOutboundTransformer(codex.Params{
		TokenProvider: tokens,
		BaseURL:       baseURL,
	})
	if err != nil {
		panic(err)
	}

	client := httpclient.NewHttpClient()
	executor := codexOutbound.CustomizeExecutor(client)

	// Turn 1: plain request, capture blob + tool call for the history.
	turn1 := buildRequest()
	httpReq1, err := codexOutbound.TransformRequest(ctx, turn1)
	if err != nil {
		panic(err)
	}
	httpReq1.URL = baseURL + "/v1/responses"
	raw1, err := executor.DoStream(ctx, httpReq1)
	if err != nil {
		fmt.Println("turn1 DoStream error:", err)
		os.Exit(1)
	}
	var events1 []*httpclient.StreamEvent
	for raw1.Next() {
		if ev := raw1.Current(); ev != nil {
			events1 = append(events1, ev)
			describe("T1RAW", ev)
		}
	}
	blob, callID, callName, callArgs := extractFromTurn1(events1)
	fmt.Printf("turn1 captured: blob_len=%d call=%s(%s)\n", len(blob), callName, callArgs)

	if blob == "" {
		fmt.Println("no reasoning blob captured; rerun")
		os.Exit(0)
	}

	// Turn 2: replay history like opencode does (reasoning item + function_call + output + user msg).
	var turn2 *llm.Request
	if os.Getenv("CONSECUTIVE") == "1" {
		turn2 = buildRequestWithConsecutiveReasoning(blob)
	} else {
		turn2 = buildRequestWithHistory(blob, callID, callName, callArgs)
	}
	httpReq2, err := codexOutbound.TransformRequest(ctx, turn2)
	if err != nil {
		panic(err)
	}
	httpReq2.URL = baseURL + "/v1/responses"
	raw2, err := executor.DoStream(ctx, httpReq2)
	if err != nil {
		fmt.Println("turn2 DoStream error:", err)
		os.Exit(1)
	}
	var rawEvents []*httpclient.StreamEvent
	for raw2.Next() {
		if ev := raw2.Current(); ev != nil {
			rawEvents = append(rawEvents, ev)
			describe("RAW", ev)
		}
	}
	if err := raw2.Err(); err != nil {
		fmt.Println("stream error:", err)
	}
	fmt.Printf("total raw events: %d\n", len(rawEvents))

	respOutbound, err := responses.NewOutboundTransformerWithConfig(&responses.Config{
		BaseURL:        baseURL,
		APIKeyProvider: auth.NewStaticKeyProvider(apiKey),
	})
	if err != nil {
		panic(err)
	}

	httpReq := httpReq2
	llmStream, err := respOutbound.TransformStream(ctx, httpReq, &sliceStream{events: rawEvents})
	if err != nil {
		panic(err)
	}

	inbound := responses.NewInboundTransformer()
	clientStream, err := inbound.TransformStream(ctx, llmStream)
	if err != nil {
		panic(err)
	}

	var clientEvents []*httpclient.StreamEvent
	for clientStream.Next() {
		if ev := clientStream.Current(); ev != nil {
			clientEvents = append(clientEvents, ev)
			describe("CLI", ev)
		}
	}
	fmt.Printf("total client events: %d\n", len(clientEvents))

	body, _, err := responses.AggregateStreamChunks(ctx, clientEvents)
	if err != nil {
		fmt.Println("aggregate error:", err)
		os.Exit(1)
	}
	fmt.Println("aggregated client output items:")
	printAggregated(body)
}

func extractFromTurn1(events []*httpclient.StreamEvent) (blob, callID, callName, callArgs string) {
	for _, ev := range events {
		var obj struct {
			Type string `json:"type"`
			Item *struct {
				Type             string  `json:"type"`
				ID               string  `json:"id"`
				CallID           string  `json:"call_id"`
				Name             string  `json:"name"`
				Arguments        string  `json:"arguments"`
				EncryptedContent *string `json:"encrypted_content"`
			} `json:"item"`
		}
		if err := json.Unmarshal(ev.Data, &obj); err != nil || obj.Item == nil {
			continue
		}
		if obj.Type == "response.output_item.done" && obj.Item.Type == "reasoning" && obj.Item.EncryptedContent != nil {
			blob = *obj.Item.EncryptedContent
		}
		if obj.Type == "response.output_item.done" && obj.Item.Type == "function_call" {
			callID, callName, callArgs = obj.Item.CallID, obj.Item.Name, obj.Item.Arguments
		}
	}
	return
}

func describe(prefix string, ev *httpclient.StreamEvent) {
	var obj struct {
		Type string `json:"type"`
		Item *struct {
			Type            string  `json:"type"`
			ID              string  `json:"id"`
			EncryptedContent *string `json:"encrypted_content"`
		} `json:"item"`
	}
	if err := json.Unmarshal(ev.Data, &obj); err != nil {
		return
	}
	if (obj.Type != "response.output_item.added" && obj.Type != "response.output_item.done") || obj.Item == nil || obj.Item.Type != "reasoning" {
		return
	}
	encLen, encPresent := 0, obj.Item.EncryptedContent != nil
	if encPresent {
		encLen = len(*obj.Item.EncryptedContent)
	}
	fmt.Printf("%s %-34s id=%.24s enc_present=%v enc_len=%d\n", prefix, obj.Type, obj.Item.ID, encPresent, encLen)
}

func printAggregated(body []byte) {
	var resp struct {
		Output []struct {
			Type             string  `json:"type"`
			ID               string  `json:"id"`
			EncryptedContent *string `json:"encrypted_content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		fmt.Println("aggregate unmarshal error:", err)
		return
	}
	for i, item := range resp.Output {
		encLen := 0
		if item.EncryptedContent != nil {
			encLen = len(*item.EncryptedContent)
		}
		fmt.Printf("  AGG output[%d] type=%s id=%.24s enc_len=%d\n", i, item.Type, item.ID, encLen)
	}
}

func buildRequest() *llm.Request {
	return &llm.Request{
		Model: "gpt-5.6-luna",
		Messages: []llm.Message{
			{
				Role: "user",
				Content: llm.MessageContent{
					Content: lo.ToPtr("Think step by step about the safest order to inspect a git repo, then use the run_cmd tool to run: git status"),
				},
			},
		},
		Stream:           lo.ToPtr(true),
		ReasoningEffort:  "high",
		ReasoningSummary: lo.ToPtr("auto"),
		PromptCacheKey:   lo.ToPtr("ses_diagprobe0002"),
		Tools: []llm.Tool{
			{
				Type: "function",
				Function: llm.Function{
					Name:        "run_cmd",
					Description: "Run a shell command",
					Parameters:  json.RawMessage(`{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}`),
				},
			},
		},
		TransformerMetadata: map[string]any{
			"include": []string{"reasoning.encrypted_content"},
		},
	}
}

// buildRequestWithHistory mimics opencode's second turn: the previous assistant
// reasoning (encrypted blob) + tool call, the tool output, and a follow-up user message.
func buildRequestWithHistory(blob, callID, callName, callArgs string) *llm.Request {
	req := buildRequest()
	req.Messages = []llm.Message{
		req.Messages[0],
		{
			Role:               "assistant",
			ReasoningSignature: lo.ToPtr(blob),
			ToolCalls: []llm.ToolCall{
				{
					ID:       callID,
					Type:     "function",
					Function: llm.FunctionCall{Name: callName, Arguments: callArgs},
				},
			},
		},
		{
			Role: "tool",
			Content: llm.MessageContent{
				Content: lo.ToPtr("On branch main\nnothing to commit, working tree clean"),
			},
			ToolCallID: lo.ToPtr(callID),
		},
		{
			Role: "user",
			Content: llm.MessageContent{
				Content: lo.ToPtr("Good. Now think again and use run_cmd to run: git log --oneline -3"),
			},
		},
	}
	return req
}

// buildRequestWithConsecutiveReasoning mimics a history where TWO reasoning items
// appear back-to-back with no function call between them (a structure AxonHub's
// request reconstruction can produce). Watch whether the upstream echoes multiple
// reasoning items or emits unusual events.
func buildRequestWithConsecutiveReasoning(blob string) *llm.Request {
	req := buildRequest()
	req.Messages = []llm.Message{
		req.Messages[0],
		{Role: "assistant", ReasoningSignature: lo.ToPtr(blob)},
		{Role: "assistant", ReasoningSignature: lo.ToPtr(blob)},
		{
			Role: "user",
			Content: llm.MessageContent{
				Content: lo.ToPtr("Now think again and use run_cmd to run: git log --oneline -3"),
			},
		},
	}
	return req
}

type sliceStream struct {
	events []*httpclient.StreamEvent
	idx    int
}

func (s *sliceStream) Next() bool {
	s.idx++
	return s.idx-1 < len(s.events)
}

func (s *sliceStream) Current() *httpclient.StreamEvent {
	if s.idx-1 >= 0 && s.idx-1 < len(s.events) {
		return s.events[s.idx-1]
	}
	return nil
}

func (s *sliceStream) Err() error   { return nil }
func (s *sliceStream) Close() error { return nil }

var _ streams.Stream[*httpclient.StreamEvent] = (*sliceStream)(nil)
