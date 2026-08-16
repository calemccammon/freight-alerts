package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// ── Tokens ────────────────────────────────────────────────────────────────────

func TestEachTokenIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		token, _, err := NewToken()
		if err != nil {
			t.Fatal(err)
		}
		if seen[token] {
			t.Fatal("NewToken repeated a value")
		}
		seen[token] = true
	}
}

func TestTheHashMatchesTheToken(t *testing.T) {
	token, hash, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if !EqualTokens(hash, HashToken(token)) {
		t.Fatal("the returned hash does not match hashing the token")
	}
}

func TestTheHashDoesNotContainTheToken(t *testing.T) {
	// The point of storing a hash is that a database dump cannot be replayed as
	// live sessions.
	token, hash, _ := NewToken()
	if strings.Contains(string(hash), token) {
		t.Fatal("the stored hash leaks the token")
	}
}

func TestDifferentTokensHashDifferently(t *testing.T) {
	if EqualTokens(HashToken("a"), HashToken("b")) {
		t.Fatal("distinct tokens collided")
	}
}

// ── Cookies ───────────────────────────────────────────────────────────────────

func cookieFrom(t *testing.T, rec *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("cookie %q not set", name)
	return nil
}

func TestTheSessionCookieIsHardened(t *testing.T) {
	rec := httptest.NewRecorder()
	SetSessionCookie(rec, "tok", true)
	c := cookieFrom(t, rec, SessionCookie)

	if !c.HttpOnly {
		t.Error("HttpOnly must be set so page scripts cannot read the session")
	}
	if !c.Secure {
		t.Error("Secure must be set when serving over https")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Error("SameSite=Lax blocks cross-site posts while allowing the OAuth redirect back")
	}
	if c.Value != "tok" {
		t.Errorf("value = %q, want the token", c.Value)
	}
}

func TestTheSecureFlagCanBeDroppedForLocalHTTP(t *testing.T) {
	rec := httptest.NewRecorder()
	SetSessionCookie(rec, "tok", false)
	if cookieFrom(t, rec, SessionCookie).Secure {
		t.Fatal("Secure should be off when explicitly running insecure locally")
	}
}

func TestClearingTheSessionExpiresTheCookie(t *testing.T) {
	rec := httptest.NewRecorder()
	ClearSessionCookie(rec, true)
	c := cookieFrom(t, rec, SessionCookie)
	if c.MaxAge >= 0 || c.Value != "" {
		t.Fatalf("logout cookie = %+v, want an immediate expiry with no value", c)
	}
}

// ── OAuth state ───────────────────────────────────────────────────────────────

func TestStateRoundTripSucceeds(t *testing.T) {
	state, err := NewState()
	if err != nil {
		t.Fatal(err)
	}
	setRec := httptest.NewRecorder()
	SetStateCookie(setRec, state, true)

	req := httptest.NewRequest(http.MethodGet, "/auth/callback", nil)
	req.AddCookie(cookieFrom(t, setRec, stateCookie))

	if err := VerifyState(httptest.NewRecorder(), req, state, true); err != nil {
		t.Fatalf("matching state rejected: %v", err)
	}
}

func TestAMismatchedStateIsRejected(t *testing.T) {
	// This is what stops a callback the user never initiated from being
	// replayed against them.
	setRec := httptest.NewRecorder()
	SetStateCookie(setRec, "expected", true)

	req := httptest.NewRequest(http.MethodGet, "/auth/callback", nil)
	req.AddCookie(cookieFrom(t, setRec, stateCookie))

	if err := VerifyState(httptest.NewRecorder(), req, "attacker-supplied", true); err == nil {
		t.Fatal("expected a state mismatch to be rejected")
	}
}

func TestAMissingStateCookieIsRejected(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/auth/callback", nil)
	if err := VerifyState(httptest.NewRecorder(), req, "anything", true); err == nil {
		t.Fatal("expected a missing state cookie to be rejected")
	}
}

func TestAnEmptyReturnedStateIsRejected(t *testing.T) {
	setRec := httptest.NewRecorder()
	SetStateCookie(setRec, "expected", true)
	req := httptest.NewRequest(http.MethodGet, "/auth/callback", nil)
	req.AddCookie(cookieFrom(t, setRec, stateCookie))

	if err := VerifyState(httptest.NewRecorder(), req, "", true); err == nil {
		t.Fatal("expected an empty state to be rejected")
	}
}

func TestVerifyingClearsTheStateCookieSoItCannotBeReplayed(t *testing.T) {
	setRec := httptest.NewRecorder()
	SetStateCookie(setRec, "once", true)
	req := httptest.NewRequest(http.MethodGet, "/auth/callback", nil)
	req.AddCookie(cookieFrom(t, setRec, stateCookie))

	clearRec := httptest.NewRecorder()
	_ = VerifyState(clearRec, req, "once", true)
	if c := cookieFrom(t, clearRec, stateCookie); c.MaxAge >= 0 {
		t.Fatal("the state cookie should be expired after one use")
	}
}

// ── GitHub flow ───────────────────────────────────────────────────────────────

func TestUnconfiguredWithoutCredentials(t *testing.T) {
	if NewGitHub("", "", "").Configured() {
		t.Fatal("no credentials should report unconfigured")
	}
	if !NewGitHub("id", "secret", "https://x/cb").Configured() {
		t.Fatal("full credentials should report configured")
	}
}

func TestAuthorizeURLCarriesTheStateAndRequestsNoScopes(t *testing.T) {
	g := NewGitHub("client-123", "secret", "https://example.test/auth/callback")
	parsed, err := url.Parse(g.AuthorizeURL("state-abc"))
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	if q.Get("client_id") != "client-123" || q.Get("state") != "state-abc" {
		t.Fatalf("unexpected query: %v", q)
	}
	// Signing in must not grant this service access to anything in the account.
	if q.Get("scope") != "" {
		t.Fatalf("scope = %q, want empty", q.Get("scope"))
	}
}

// githubStub stands in for GitHub's two endpoints.
func githubStub(t *testing.T, tokenBody, userBody string, userStatus int) *GitHub {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q; without JSON GitHub answers form-encoded", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(tokenBody))
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("identity request missing bearer token")
		}
		w.WriteHeader(userStatus)
		_, _ = w.Write([]byte(userBody))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	g := NewGitHub("id", "secret", "https://example.test/cb")
	g.tokenURL = srv.URL + "/token"
	g.userURL = srv.URL + "/user"
	return g
}

func TestExchangeReturnsTheIdentity(t *testing.T) {
	g := githubStub(t, `{"access_token":"gho_abc"}`, `{"id":4242,"login":"calemccammon"}`, http.StatusOK)
	id, err := g.Exchange(context.Background(), "code-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id.ID != 4242 || id.Login != "calemccammon" {
		t.Fatalf("got %+v, want the stub identity", id)
	}
}

func TestARejectedCodeIsAnErrorDespiteHTTP200(t *testing.T) {
	// GitHub reports a bad or reused code with a 200 and an error field, so the
	// status alone does not mean the exchange worked.
	g := githubStub(t,
		`{"error":"bad_verification_code","error_description":"The code is incorrect."}`,
		`{"id":1}`, http.StatusOK)
	_, err := g.Exchange(context.Background(), "stale")
	if err == nil || !strings.Contains(err.Error(), "bad_verification_code") {
		t.Fatalf("got %v, want the rejection surfaced", err)
	}
}

func TestAMissingAccessTokenIsAnError(t *testing.T) {
	g := githubStub(t, `{}`, `{"id":1}`, http.StatusOK)
	if _, err := g.Exchange(context.Background(), "code"); err == nil {
		t.Fatal("expected an empty token response to error")
	}
}

func TestAnUnauthorizedIdentityRequestIsAnError(t *testing.T) {
	g := githubStub(t, `{"access_token":"gho_abc"}`, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
	if _, err := g.Exchange(context.Background(), "code"); err == nil {
		t.Fatal("expected a 401 from the identity endpoint to error")
	}
}

func TestAnIdentityWithoutAnIDIsRejected(t *testing.T) {
	// github_id is the stable key for a user; a response without one would
	// create a broken account row.
	g := githubStub(t, `{"access_token":"gho_abc"}`, `{"login":"ghost"}`, http.StatusOK)
	if _, err := g.Exchange(context.Background(), "code"); err == nil {
		t.Fatal("expected an identity with no id to be rejected")
	}
}

func TestTheCodeAndSecretAreSentAsForMEncodedBody(t *testing.T) {
	var form url.Values
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.PostForm
		_, _ = w.Write([]byte(`{"access_token":"gho_abc"}`))
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Identity{ID: 1, Login: "x"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	g := NewGitHub("id", "secret", "https://example.test/cb")
	g.tokenURL, g.userURL = srv.URL+"/token", srv.URL+"/user"
	if _, err := g.Exchange(context.Background(), "the-code"); err != nil {
		t.Fatal(err)
	}
	if form.Get("code") != "the-code" || form.Get("client_secret") != "secret" {
		t.Fatalf("token request form = %v", form)
	}
}
