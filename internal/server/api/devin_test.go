package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/oauth"
	"github.com/looplj/axonhub/llm/transformer/devin"
)

func devinTestRequest(t *testing.T, h *DevinHandlers, path, body string, userID int, owner bool) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		if userID > 0 {
			ctx := contexts.WithUser(c.Request.Context(), &ent.User{ID: userID, IsOwner: owner})
			c.Request = c.Request.WithContext(authz.NewUserContext(ctx, userID))
		}
		c.Next()
	})
	router.POST("/start", h.StartOAuth)
	router.POST("/exchange", h.Exchange)
	request := httptest.NewRequest("POST", path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func startDevinTestSession(t *testing.T, h *DevinHandlers) StartDevinOAuthResponse {
	t.Helper()
	response := devinTestRequest(t, h, "/start", `{}`, 1, true)
	require.Equal(t, 200, response.Code, response.Body.String())
	require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	var result StartDevinOAuthResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
	return result
}

func devinExchangeBody(t *testing.T, session, code string) string {
	t.Helper()
	body, err := json.Marshal(ExchangeDevinOAuthRequest{SessionID: session, CallbackURL: code})
	require.NoError(t, err)
	return string(body)
}

func TestDevinOAuthFlow(t *testing.T) {
	var verifier, receivedCode string
	calls := 0
	client := httpclient.NewHttpClientWithClient(&http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		require.Equal(t, devin.OAuthTokenURL, request.URL.String())
		require.Equal(t, "POST", request.Method)
		require.Equal(t, "application/json", request.Header.Get("Content-Type"))
		var payload map[string]string
		require.NoError(t, json.NewDecoder(request.Body).Decode(&payload))
		verifier, receivedCode = payload["code_verifier"], payload["code"]
		require.NotEmpty(t, verifier)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"token":"test-session"}`))}, nil
	})})
	h := NewDevinHandlers(DevinHandlersParams{CacheConfig: xcache.Config{Mode: xcache.ModeMemory}, HttpClient: client})
	start := startDevinTestSession(t, h)
	parsed, err := url.Parse(start.AuthURL)
	require.NoError(t, err)
	require.Equal(t, devin.OAuthAuthorizeURL, parsed.Scheme+"://"+parsed.Host+parsed.Path)
	require.Equal(t, "1", parsed.Query().Get("cli_pkce_marker"))
	require.Empty(t, parsed.Query().Get("redirect_uri"))
	require.Equal(t, "S256", parsed.Query().Get("code_challenge_method"))
	require.Equal(t, start.SessionID, parsed.Query().Get("state"))
	require.NotContains(t, start.AuthURL, "code_verifier")
	// A second administrator cannot exchange another user's session.
	foreign := devinTestRequest(t, h, "/exchange", devinExchangeBody(t, start.SessionID, "one-time-code"), 2, true)
	require.Equal(t, 400, foreign.Code)
	require.Zero(t, calls)
	// Invalid callback state does not destroy a valid session.
	wrong := devinTestRequest(t, h, "/exchange", devinExchangeBody(t, start.SessionID, "http://localhost/callback?code=one-time-code&state=wrong"), 1, true)
	require.Equal(t, 400, wrong.Code)
	result := devinTestRequest(t, h, "/exchange", devinExchangeBody(t, start.SessionID, "one-time-code"), 1, true)
	require.Equal(t, 200, result.Code, result.Body.String())
	require.Equal(t, "no-store", result.Header().Get("Cache-Control"))
	var credentials ExchangeDevinOAuthResponse
	require.NoError(t, json.Unmarshal(result.Body.Bytes(), &credentials))
	require.Equal(t, "devin-session-token$test-session", credentials.Credentials)
	require.Equal(t, "one-time-code", receivedCode)
	require.Equal(t, oauth.GenerateCodeChallenge(verifier), parsed.Query().Get("code_challenge"))
	// PKCE session is consumed exactly once, before the upstream call.
	replay := devinTestRequest(t, h, "/exchange", devinExchangeBody(t, start.SessionID, "one-time-code"), 1, true)
	require.Equal(t, 400, replay.Code)
	require.Equal(t, 1, calls)
}

func TestDevinOAuthUpstreamFailuresDoNotLeakOrReplay(t *testing.T) {
	for _, upstream := range []struct {
		status int
		body   string
	}{
		{401, `{"token":"secret-leak","code_verifier":"private-verifier"}`},
		{200, `not-json-private-verifier`},
		{200, `{"token":""}`},
		{200, `{"token":"line\nbreak"}`},
	} {
		calls := 0
		client := httpclient.NewHttpClientWithClient(&http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: upstream.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(upstream.body))}, nil
		})})
		h := NewDevinHandlers(DevinHandlersParams{CacheConfig: xcache.Config{Mode: xcache.ModeMemory}, HttpClient: client})
		session := startDevinTestSession(t, h)
		body := devinExchangeBody(t, session.SessionID, "authorization-code")
		result := devinTestRequest(t, h, "/exchange", body, 1, true)
		require.Equal(t, 502, result.Code)
		require.NotContains(t, result.Body.String(), "secret-leak")
		require.NotContains(t, result.Body.String(), "private-verifier")
		require.NotContains(t, result.Body.String(), "authorization-code")
		replay := devinTestRequest(t, h, "/exchange", body, 1, true)
		require.Equal(t, 400, replay.Code)
		require.Equal(t, 1, calls)
	}
}

func TestDevinOAuthAuthorizationAndExpiry(t *testing.T) {
	h := NewDevinHandlers(DevinHandlersParams{CacheConfig: xcache.Config{Mode: xcache.ModeMemory}, HttpClient: httpclient.NewHttpClient()})
	for _, endpoint := range []string{"/start", "/exchange"} {
		require.Equal(t, 401, devinTestRequest(t, h, endpoint, `{}`, 0, false).Code)
		require.Equal(t, 403, devinTestRequest(t, h, endpoint, `{}`, 3, false).Code)
		require.Equal(t, 400, devinTestRequest(t, h, endpoint, `{`, 1, true).Code)
	}
	session := startDevinTestSession(t, h)
	ctx := httptest.NewRequest("GET", "/", nil).Context()
	state, err := h.stateCache.Get(ctx, devinOAuthCacheKey(session.SessionID))
	require.NoError(t, err)
	state.CreatedAt = time.Now().Add(-devinOAuthTTL - time.Minute).Unix()
	require.NoError(t, h.stateCache.Set(ctx, devinOAuthCacheKey(session.SessionID), state))
	result := devinTestRequest(t, h, "/exchange", devinExchangeBody(t, session.SessionID, "code"), 1, true)
	require.Equal(t, 400, result.Code)
	require.Contains(t, result.Body.String(), "expired")
}

func TestParseDevinOAuthCode(t *testing.T) {
	for _, input := range []string{" code-123 ", "http://127.0.0.1/callback?state=session&code=code-123", "https://app.example.invalid/callback?state=session&code=code-123"} {
		code, err := parseDevinOAuthCode(input, "session")
		require.NoError(t, err)
		require.Equal(t, "code-123", code)
	}
	for _, input := range []string{"", "\n", "two words", "devin-session-token$token", "https://localhost/?code=code", "https://localhost/?code=a&code=b&state=session", "ftp://localhost/?code=code&state=session", string(bytes.Repeat([]byte("x"), 8193))} {
		_, err := parseDevinOAuthCode(input, "session")
		require.Error(t, err)
	}
}
