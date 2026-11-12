package devin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/samber/lo"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/auth"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
)

const connectContentType = "application/connect+proto"

type Config struct {
	BaseURL        string
	EndpointPath   string
	APIKeyProvider auth.APIKeyProvider
}

type OutboundTransformer struct{ config Config }

func NewOutboundTransformerWithConfig(config *Config) (*OutboundTransformer, error) {
	if config == nil || config.APIKeyProvider == nil {
		return nil, fmt.Errorf("Devin session token provider is required")
	}
	c := *config
	if c.BaseURL == "" {
		c.BaseURL = DevinDefaultBaseURL
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, fmt.Errorf("invalid Devin base URL")
	}
	if c.EndpointPath == "" {
		c.EndpointPath = DevinChatPath
	}
	return &OutboundTransformer{config: c}, nil
}

func (*OutboundTransformer) APIFormat() llm.APIFormat { return llm.APIFormatDevinChat }

func (*OutboundTransformer) AllowPassThroughBody(context.Context, *llm.Request, *httpclient.Request) bool {
	return false
}

func (t *OutboundTransformer) CustomizeExecutor(inner pipeline.Executor) pipeline.Executor {
	return &executor{inner: inner}
}

func (t *OutboundTransformer) TransformRequest(ctx context.Context, req *llm.Request) (*httpclient.Request, error) {
	if req == nil {
		return nil, fmt.Errorf("%w: request is nil", transformer.ErrInvalidRequest)
	}
	if _, _, _, err := convertMessages(req); err != nil {
		return nil, err
	}
	if req.RequestType != "" && req.RequestType != llm.RequestTypeChat {
		return nil, fmt.Errorf("%w: Devin only supports chat", transformer.ErrInvalidRequest)
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, fmt.Errorf("%w: model is required", transformer.ErrInvalidRequest)
	}
	token := NormalizeToken(t.config.APIKeyProvider.Get(ctx))
	if token == "" {
		return nil, fmt.Errorf("%w: Devin session token is required", transformer.ErrInvalidRequest)
	}
	// Keep a credential-free JSON body for overrides and persistence. The executor
	// serializes it to Connect-RPC only after all middleware has finished.
	copy := *req
	copy.RawRequest = nil
	copy.TransformerMetadata = nil
	copy.ExtraBody = nil
	body, err := json.Marshal(&copy)
	if err != nil {
		return nil, fmt.Errorf("marshal Devin request: %w", err)
	}
	session := ""
	if req.PromptCacheKey != nil {
		session = *req.PromptCacheKey
	}
	if session == "" {
		session = req.Metadata["session_id"]
	}
	if session == "" {
		session = uuid.NewString()
	}
	session = uuid.NewSHA1(uuid.NameSpaceOID, []byte(session)).String()
	return &httpclient.Request{
		Method: http.MethodPost, URL: t.config.BaseURL + t.config.EndpointPath,
		Headers: make(http.Header), ContentType: "application/json", Body: body,
		Auth:        &httpclient.AuthConfig{Type: httpclient.AuthTypeAPIKey, HeaderKey: "Authorization", APIKey: token},
		RequestType: llm.RequestTypeChat.String(), APIFormat: t.APIFormat().String(),
		SkipInboundQueryMerge: true,
		Metadata:              map[string]string{"devin_session": session, httpclient.MetadataStreamDecoderContentType: connectContentType},
	}, nil
}

func NormalizeToken(token string) string {
	token = strings.TrimSpace(token)
	if token == "" || strings.HasPrefix(token, "devin-session-token$") {
		return token
	}
	return "devin-session-token$" + token
}

func convertMessages(req *llm.Request) (string, []DevinPrompt, []DevinTool, error) {
	var system []string
	var prompts []DevinPrompt
	var tools []DevinTool
	for _, msg := range req.Messages {
		p := DevinPrompt{MessageID: msg.ID}
		if _, err := uuid.Parse(p.MessageID); err != nil {
			p.MessageID = ""
		}
		if len(msg.Content.MultipleContent) == 0 && msg.Content.Content != nil {
			p.Content = *msg.Content.Content
		}
		for _, part := range msg.Content.MultipleContent {
			switch part.Type {
			case "text":
				if part.Text != nil {
					p.Content += *part.Text
				}
			case "image_url":
				if part.ImageURL == nil {
					return "", nil, nil, fmt.Errorf("%w: image is missing", transformer.ErrInvalidRequest)
				}
				header, data, ok := strings.Cut(part.ImageURL.URL, ",")
				if !ok || !strings.HasPrefix(header, "data:image/") || !strings.HasSuffix(header, ";base64") {
					return "", nil, nil, fmt.Errorf("%w: Devin requires base64 data URL images; remote images are not supported", transformer.ErrInvalidRequest)
				}
				if _, err := base64.StdEncoding.DecodeString(data); err != nil {
					return "", nil, nil, fmt.Errorf("%w: invalid base64 image", transformer.ErrInvalidRequest)
				}
				p.Images = append(p.Images, DevinImage{Base64Data: data, MimeType: strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64")})
			default:
				return "", nil, nil, fmt.Errorf("%w: Devin does not support content type %q", transformer.ErrInvalidRequest, part.Type)
			}
		}
		switch msg.Role {
		case "system", "developer":
			if len(p.Images) > 0 {
				return "", nil, nil, fmt.Errorf("%w: system images are not supported", transformer.ErrInvalidRequest)
			}
			system = append(system, p.Content)
			continue
		case "user":
			p.Source = 1
		case "assistant":
			p.Source = 2
		case "tool":
			p.Source = 4
			if msg.ToolCallID == nil || *msg.ToolCallID == "" {
				return "", nil, nil, fmt.Errorf("%w: tool result has no call ID", transformer.ErrInvalidRequest)
			}
			p.ToolCallID = decodeToolID(*msg.ToolCallID)
		default:
			return "", nil, nil, fmt.Errorf("%w: unsupported role %q", transformer.ErrInvalidRequest, msg.Role)
		}
		if msg.ReasoningContent != nil {
			p.Thinking = *msg.ReasoningContent
		} else if msg.Reasoning != nil {
			p.Thinking = *msg.Reasoning
		}
		if msg.ReasoningSignature != nil {
			p.Signature, p.SignatureType = decodeSignature(*msg.ReasoningSignature)
		}
		for _, call := range msg.ToolCalls {
			if call.Type != "" && call.Type != "function" {
				return "", nil, nil, fmt.Errorf("%w: Devin only supports function tools", transformer.ErrInvalidRequest)
			}
			p.ToolCalls = append(p.ToolCalls, DevinToolCall{ID: decodeToolID(call.ID), Name: call.Function.Name, Arguments: call.Function.Arguments})
		}
		if p.Source == 1 && strings.TrimSpace(p.Content) == "" && len(p.Images) == 0 {
			return "", nil, nil, fmt.Errorf("%w: empty user turn", transformer.ErrInvalidRequest)
		}
		prompts = append(prompts, p)
	}
	if len(prompts) == 0 {
		return "", nil, nil, fmt.Errorf("%w: at least one conversation message is required", transformer.ErrInvalidRequest)
	}
	for _, tool := range req.Tools {
		if tool.Type != "function" {
			return "", nil, nil, fmt.Errorf("%w: Devin only supports function tools", transformer.ErrInvalidRequest)
		}
		params := tool.Function.Parameters
		if len(params) == 0 {
			params = tool.Function.ParametersJsonSchema
		}
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		if tool.Function.Name == "" || !json.Valid(params) {
			return "", nil, nil, fmt.Errorf("%w: invalid function tool", transformer.ErrInvalidRequest)
		}
		tools = append(tools, DevinTool{Name: tool.Function.Name, Description: tool.Function.Description, Parameters: params})
	}
	if req.ToolChoice != nil {
		if req.ToolChoice.ToolChoice != nil && *req.ToolChoice.ToolChoice == "none" {
			tools = nil
		} else if req.ToolChoice.NamedToolChoice != nil || (req.ToolChoice.ToolChoice != nil && *req.ToolChoice.ToolChoice != "auto") || len(req.ToolChoice.Tools) > 0 {
			return "", nil, nil, fmt.Errorf("%w: Devin does not support forced tool choice", transformer.ErrInvalidRequest)
		}
	}
	return strings.Join(system, "\n\n"), prompts, tools, nil
}

const signaturePrefix = "axonhub-devin:"

func decodeSignature(value string) ([]byte, string) {
	if strings.HasPrefix(value, signaturePrefix) {
		var sig struct {
			Data []byte
			Type string
		}
		data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, signaturePrefix))
		if err == nil && json.Unmarshal(data, &sig) == nil {
			return sig.Data, sig.Type
		}
	}
	return []byte(value), ""
}

func encodeSignature(data []byte, typ string) string {
	if typ == "" && utf8.Valid(data) && !strings.HasPrefix(string(data), signaturePrefix) {
		return string(data)
	}
	value, _ := json.Marshal(struct {
		Data []byte
		Type string
	}{data, typ})
	return signaturePrefix + base64.StdEncoding.EncodeToString(value)
}

type executor struct{ inner pipeline.Executor }

func prepareWire(req *httpclient.Request) (*httpclient.Request, error) {
	var input llm.Request
	if err := json.Unmarshal(req.Body, &input); err != nil {
		return nil, fmt.Errorf("%w: invalid Devin request body: %w", transformer.ErrInvalidRequest, err)
	}
	system, prompts, tools, err := convertMessages(&input)
	if err != nil {
		return nil, err
	}
	if input.RequestType != "" && input.RequestType != llm.RequestTypeChat {
		return nil, fmt.Errorf("%w: Devin only supports chat", transformer.ErrInvalidRequest)
	}
	token := req.Headers.Get("Authorization")
	if req.Auth != nil {
		token = req.Auth.APIKey
	}
	token = NormalizeToken(token)
	if token == "" {
		return nil, fmt.Errorf("missing Devin session token")
	}
	model := ResolveDevinChatModelUID(input.Model, input.ReasoningEffort, int(lo.FromPtr(input.ReasoningBudget)))
	maxTokens := int(lo.FromPtr(input.MaxCompletionTokens))
	if maxTokens <= 0 {
		maxTokens = int(lo.FromPtr(input.MaxTokens))
	}
	if info := lookupModel(strings.TrimPrefix(input.Model, "devin/")); info != nil && info.MaxCompletionTokens > 0 {
		if maxTokens <= 0 || maxTokens > info.MaxCompletionTokens {
			maxTokens = info.MaxCompletionTokens
		}
	}
	session := req.Metadata["devin_session"]
	if session == "" {
		session = uuid.NewString()
	}
	body := BuildDevinGetChatMessageRequest(token, "", model, system, prompts, tools, input.Temperature, maxTokens, session, session)
	copy := *req
	copy.Headers = req.Headers.Clone()
	if copy.Headers == nil {
		copy.Headers = make(http.Header)
	}
	copy.Headers.Set("Authorization", "Basic "+token+"-"+token)
	copy.Headers.Set("Content-Type", connectContentType)
	copy.Headers.Set("Connect-Protocol-Version", "1")
	copy.Headers.Set("Accept", "*/*")
	copy.Headers.Set("Accept-Encoding", "identity")
	copy.Headers["User-Agent"] = []string{""}
	copy.Headers.Set("Sentry-Trace", GenerateDevinSentryTrace())
	copy.Auth = nil
	copy.DisableCompression = true
	copy.ContentType = connectContentType
	copy.Body = WrapConnectEnvelope(body)
	return &copy, nil
}

func (e *executor) Do(ctx context.Context, req *httpclient.Request) (*httpclient.Response, error) {
	wire, err := prepareWire(req)
	if err != nil {
		return nil, err
	}
	response, err := e.inner.Do(ctx, wire)
	if response != nil {
		response.Request = req
	}
	return response, err
}

func (e *executor) DoStream(ctx context.Context, req *httpclient.Request) (streams.Stream[*httpclient.StreamEvent], error) {
	wire, err := prepareWire(req)
	if err != nil {
		return nil, err
	}
	return e.inner.DoStream(ctx, wire)
}

func (t *OutboundTransformer) TransformResponse(ctx context.Context, response *httpclient.Response) (*llm.Response, error) {
	if response == nil {
		return nil, fmt.Errorf("Devin response is nil")
	}
	if response.StatusCode >= 400 {
		return nil, t.TransformError(ctx, &httpclient.Error{StatusCode: response.StatusCode, Body: response.Body, Headers: response.Headers})
	}
	decoder := newDecoder(ctx, ioReadCloser{bytes.NewReader(response.Body)})
	state := newResponseState(response.Request)
	for decoder.Next() {
		if _, err := state.consume(decoder.Current()); err != nil {
			return nil, err
		}
	}
	if err := decoder.Err(); err != nil {
		return nil, err
	}
	return state.response(), nil
}

type ioReadCloser struct{ *bytes.Reader }

func (ioReadCloser) Close() error { return nil }

func (*OutboundTransformer) TransformError(_ context.Context, err *httpclient.Error) *llm.ResponseError {
	if err == nil {
		return &llm.ResponseError{StatusCode: 502, Detail: llm.ErrorDetail{Type: "api_error", Message: "Devin upstream error"}}
	}
	var detail struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(err.Body, &detail)
	if detail.Message == "" {
		detail.Message = http.StatusText(err.StatusCode)
	}
	return &llm.ResponseError{StatusCode: err.StatusCode, Detail: llm.ErrorDetail{Type: "api_error", Code: detail.Code, Message: detail.Message}, Cause: err}
}

var _ transformer.Outbound = (*OutboundTransformer)(nil)
var _ pipeline.ChannelCustomizedExecutor = (*OutboundTransformer)(nil)
