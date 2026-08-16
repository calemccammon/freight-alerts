package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// GitHub implements the authorization-code half of OAuth 2.0 against GitHub.
//
// Written against net/http rather than pulling in golang.org/x/oauth2: for a
// single provider the flow is two POSTs, and having it spelled out here means
// the state check, the token exchange, and the identity fetch are all readable
// in one file rather than delegated to a library's abstractions.
type GitHub struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
	http         *http.Client

	// Overridable so tests can point the flow at a local server.
	authorizeURL string
	tokenURL     string
	userURL      string
}

func NewGitHub(clientID, clientSecret, redirectURL string) *GitHub {
	return &GitHub{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		http:         &http.Client{Timeout: 10 * time.Second},
		authorizeURL: "https://github.com/login/oauth/authorize",
		tokenURL:     "https://github.com/login/oauth/access_token",
		userURL:      "https://api.github.com/user",
	}
}

// Configured reports whether sign-in is possible. The service still runs
// without credentials -- the poller does not need them -- so this lets the API
// return a clear error instead of a broken redirect.
func (g *GitHub) Configured() bool {
	return g.ClientID != "" && g.ClientSecret != "" && g.RedirectURL != ""
}

// AuthorizeURL is where the browser is sent to sign in.
func (g *GitHub) AuthorizeURL(state string) string {
	q := url.Values{
		"client_id":    {g.ClientID},
		"redirect_uri": {g.RedirectURL},
		"state":        {state},
		// No scopes requested. The service only needs to know who the user is,
		// and an empty scope still yields the public profile -- so signing in
		// grants this service no access to anything in the user's account.
		"scope": {""},
	}
	return g.authorizeURL + "?" + q.Encode()
}

type Identity struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

// Exchange trades the callback code for an access token, then resolves the
// identity behind it.
func (g *GitHub) Exchange(ctx context.Context, code string) (Identity, error) {
	token, err := g.accessToken(ctx, code)
	if err != nil {
		return Identity{}, err
	}
	return g.identity(ctx, token)
}

func (g *GitHub) accessToken(ctx context.Context, code string) (string, error) {
	form := url.Values{
		"client_id":     {g.ClientID},
		"client_secret": {g.ClientSecret},
		"code":          {code},
		"redirect_uri":  {g.RedirectURL},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.tokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Without this GitHub answers form-encoded, which is easy to mis-parse.
	req.Header.Set("Accept", "application/json")

	resp, err := g.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token exchange returned %d", resp.StatusCode)
	}

	var payload struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	// GitHub reports a bad or reused code with HTTP 200 and an error field, so
	// the status alone does not mean the exchange worked.
	if payload.Error != "" {
		return "", fmt.Errorf("token exchange rejected: %s (%s)", payload.Error, payload.Description)
	}
	if payload.AccessToken == "" {
		return "", fmt.Errorf("token exchange returned no access token")
	}
	return payload.AccessToken, nil
}

func (g *GitHub) identity(ctx context.Context, token string) (Identity, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.userURL, nil)
	if err != nil {
		return Identity{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := g.http.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("fetch identity: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return Identity{}, fmt.Errorf("identity request returned %d: %s", resp.StatusCode, snippet)
	}

	var id Identity
	if err := json.NewDecoder(resp.Body).Decode(&id); err != nil {
		return Identity{}, fmt.Errorf("decode identity: %w", err)
	}
	if id.ID == 0 {
		return Identity{}, fmt.Errorf("identity response had no user id")
	}
	return id, nil
}
