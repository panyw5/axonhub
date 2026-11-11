package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/fx"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/scopes"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/oauth"
	"github.com/looplj/axonhub/llm/transformer/devin"
)

const devinOAuthTTL = 10 * time.Minute

type DevinHandlersParams struct {
	fx.In
	CacheConfig xcache.Config
	HttpClient  *httpclient.HttpClient
}

type DevinHandlers struct {
	stateCache xcache.Cache[devinOAuthState]
	httpClient *httpclient.HttpClient
	consumeMu  sync.Mutex
}

type devinOAuthState struct {
	CodeVerifier string `json:"code_verifier"`
	UserID       int    `json:"user_id"`
	CreatedAt    int64  `json:"created_at"`
}

type StartDevinOAuthResponse struct {
	SessionID string `json:"session_id"`
	AuthURL   string `json:"auth_url"`
}

type ExchangeDevinOAuthRequest struct {
	SessionID   string                  `json:"session_id" binding:"required"`
	CallbackURL string                  `json:"callback_url" binding:"required"`
	Proxy       *httpclient.ProxyConfig `json:"proxy,omitempty"`
}

type ExchangeDevinOAuthResponse struct {
	Credentials string `json:"credentials"`
}

func NewDevinHandlers(params DevinHandlersParams) *DevinHandlers {
	return &DevinHandlers{stateCache: xcache.NewFromConfig[devinOAuthState](params.CacheConfig), httpClient: params.HttpClient}
}

func devinOAuthCacheKey(session string) string { return "devin:oauth:v1:" + session }

func requireDevinOAuthUser(c *gin.Context) (int, bool) {
	ctx := c.Request.Context()
	user, ok := contexts.GetUser(ctx)
	if !ok || user.ID <= 0 {
		JSONError(c, http.StatusUnauthorized, errors.New("authentication required"))
		return 0, false
	}
	if err := authz.RequireScope(ctx, scopes.ScopeWriteChannels); err != nil {
		JSONError(c, http.StatusForbidden, errors.New("channel write permission required"))
		return 0, false
	}
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	return user.ID, true
}

// StartOAuth creates a user-bound PKCE session, never exposing its verifier.
func (h *DevinHandlers) StartOAuth(c *gin.Context) {
	userID, ok := requireDevinOAuthUser(c)
	if !ok {
		return
	}
	var input struct{}
	if c.ShouldBindJSON(&input) != nil {
		JSONError(c, 400, errors.New("invalid request format"))
		return
	}
	state, err := oauth.GenerateState(32)
	if err != nil {
		JSONError(c, 500, errors.New("could not generate OAuth state"))
		return
	}
	verifier, err := oauth.GenerateCodeVerifier(64)
	if err != nil {
		JSONError(c, 500, errors.New("could not generate PKCE verifier"))
		return
	}
	if err := h.stateCache.Set(c.Request.Context(), devinOAuthCacheKey(state), devinOAuthState{
		UserID: userID, CodeVerifier: verifier, CreatedAt: time.Now().Unix(),
	}, xcache.WithExpiration(devinOAuthTTL)); err != nil {
		JSONError(c, 500, errors.New("could not save OAuth session"))
		return
	}
	c.JSON(200, StartDevinOAuthResponse{SessionID: state, AuthURL: devin.OAuthAuthorizationURL(oauth.GenerateCodeChallenge(verifier), state)})
}

// parseDevinOAuthCode accepts the code shown by Devin or a complete callback
// URL. Callback URLs require matching state; bare codes are bound by PKCE.
func parseDevinOAuthCode(input, session string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" || len(input) > 8192 {
		return "", errors.New("a Devin authorization code is required")
	}
	if strings.HasPrefix(input, "http://") || strings.HasPrefix(input, "https://") {
		u, err := url.Parse(input)
		if err != nil || u.Host == "" || u.User != nil {
			return "", errors.New("invalid Devin callback URL")
		}
		q, err := url.ParseQuery(u.RawQuery)
		if err != nil || len(q["code"]) != 1 || len(q["state"]) != 1 || q.Get("state") != session || q.Get("error") != "" {
			return "", errors.New("invalid Devin callback code or OAuth state mismatch")
		}
		input = q.Get("code")
	}
	if input == "" || strings.ContainsAny(input, "\r\n\t ") || strings.Contains(input, "://") || strings.HasPrefix(input, "devin-session-token$") {
		return "", errors.New("paste the Devin authorization code, not a session token")
	}
	return input, nil
}

func (h *DevinHandlers) Exchange(c *gin.Context) {
	userID, ok := requireDevinOAuthUser(c)
	if !ok {
		return
	}
	var input ExchangeDevinOAuthRequest
	if c.ShouldBindJSON(&input) != nil {
		JSONError(c, 400, errors.New("invalid request format"))
		return
	}
	code, err := parseDevinOAuthCode(input.CallbackURL, input.SessionID)
	if err != nil {
		JSONError(c, 400, err)
		return
	}
	ctx := c.Request.Context()
	state, err := h.consumeState(c, userID, input.SessionID)
	if err != nil {
		JSONError(c, 400, err)
		return
	}
	client := h.httpClient
	if input.Proxy != nil {
		client = client.WithProxy(input.Proxy)
	}
	token, err := devin.ExchangeOAuthCode(ctx, client, code, state.CodeVerifier)
	if err != nil {
		JSONError(c, 502, fmt.Errorf("Devin authorization failed: %w; start a new authorization session to retry", err))
		return
	}
	c.JSON(200, ExchangeDevinOAuthResponse{Credentials: token})
}

func (h *DevinHandlers) consumeState(c *gin.Context, userID int, session string) (devinOAuthState, error) {
	h.consumeMu.Lock()
	defer h.consumeMu.Unlock()
	ctx := c.Request.Context()
	key := devinOAuthCacheKey(session)
	state, err := h.stateCache.Get(ctx, key)
	if err != nil || state.UserID != userID || state.CodeVerifier == "" || time.Now().Unix()-state.CreatedAt >= int64(devinOAuthTTL/time.Second) {
		return devinOAuthState{}, errors.New("invalid or expired Devin OAuth session; start a new authorization session")
	}
	if err := h.stateCache.Delete(ctx, key); err != nil {
		return devinOAuthState{}, errors.New("could not consume Devin OAuth session")
	}
	return state, nil
}
