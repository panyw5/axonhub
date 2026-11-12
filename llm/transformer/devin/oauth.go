package devin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/looplj/axonhub/llm/httpclient"
)

const (
	OAuthAuthorizeURL = "https://app.devin.ai/auth/cli/continue"
	OAuthTokenURL     = "https://api.devin.ai/auth/cli/token"
)

// OAuthAuthorizationURL uses Devin's headless PKCE flow. The authorization page
// displays a code, so deployments do not need a publicly accessible callback.
func OAuthAuthorizationURL(challenge, state string) string {
	return OAuthAuthorizeURL + "?state=" + url.QueryEscape(state) +
		"&prompt=select_account&code_challenge=" + url.QueryEscape(challenge) +
		"&code_challenge_method=S256&cli_pkce_marker=1"
}

func ExchangeOAuthCode(ctx context.Context, client *httpclient.HttpClient, code, verifier string) (string, error) {
	if strings.TrimSpace(code) == "" || verifier == "" {
		return "", errors.New("authorization code and PKCE verifier are required")
	}
	body, err := json.Marshal(map[string]string{"code": strings.TrimSpace(code), "code_verifier": verifier})
	if err != nil {
		return "", fmt.Errorf("encode Devin token request: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	response, err := client.Do(ctx, &httpclient.Request{
		Method: http.MethodPost, URL: OAuthTokenURL, ContentType: "application/json",
		Headers: http.Header{"Accept": {"application/json"}}, Body: body,
	})
	if err != nil {
		var upstream *httpclient.Error
		if errors.As(err, &upstream) {
			return "", fmt.Errorf("Devin token exchange failed (HTTP %d)", upstream.StatusCode)
		}
		// Do not expose authorization codes or upstream response bodies to callers.
		return "", errors.New("could not connect to Devin token exchange")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("Devin token exchange failed (HTTP %d)", response.StatusCode)
	}
	var result struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(response.Body, &result) != nil {
		return "", errors.New("invalid Devin token exchange response")
	}
	token := strings.TrimSpace(result.Token)
	if token == "" || strings.ContainsAny(token, "\r\n\t ") {
		return "", errors.New("Devin token exchange did not return a valid session token")
	}
	return NormalizeToken(token), nil
}
