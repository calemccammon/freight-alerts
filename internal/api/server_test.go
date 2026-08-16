package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/calemccammon/freight-alerts/internal/auth"
	"github.com/calemccammon/freight-alerts/internal/rules"
	"github.com/calemccammon/freight-alerts/internal/store"
)

// fakeStore implements the narrow Store interface this package declares, which
// is why none of these handler tests need a database.
type fakeStore struct {
	users    map[int64]store.User
	sessions map[string]int64 // hex(hash) -> user id
	rules    map[int64]rules.Rule
	alerts   []store.Alert
	nextID   int64
	failOn   string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		users:    map[int64]store.User{1: {ID: 1, GitHubID: 42, Login: "cale"}},
		sessions: map[string]int64{},
		rules:    map[int64]rules.Rule{},
		nextID:   1,
	}
}

func key(hash []byte) string { return string(hash) }

func (f *fakeStore) UpsertUser(_ context.Context, githubID int64, login string) (store.User, error) {
	if f.failOn == "UpsertUser" {
		return store.User{}, errors.New("boom")
	}
	u := store.User{ID: 1, GitHubID: githubID, Login: login}
	f.users[1] = u
	return u, nil
}

func (f *fakeStore) CreateSession(_ context.Context, userID int64, hash []byte, _ time.Time) error {
	f.sessions[key(hash)] = userID
	return nil
}

func (f *fakeStore) UserBySession(_ context.Context, hash []byte) (store.User, error) {
	id, ok := f.sessions[key(hash)]
	if !ok {
		return store.User{}, store.ErrNotFound
	}
	return f.users[id], nil
}

func (f *fakeStore) DeleteSession(_ context.Context, hash []byte) error {
	delete(f.sessions, key(hash))
	return nil
}

func (f *fakeStore) CreateRule(_ context.Context, r rules.Rule) (rules.Rule, error) {
	if f.failOn == "CreateRule" {
		return rules.Rule{}, errors.New("boom")
	}
	f.nextID++
	r.ID = f.nextID
	f.rules[r.ID] = r
	return r, nil
}

func (f *fakeStore) ListRules(_ context.Context, userID int64) ([]rules.Rule, error) {
	var out []rules.Rule
	for _, r := range f.rules {
		if r.UserID == userID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeStore) DeleteRule(_ context.Context, userID, ruleID int64) error {
	r, ok := f.rules[ruleID]
	if !ok || r.UserID != userID {
		return store.ErrNotFound
	}
	delete(f.rules, ruleID)
	return nil
}

func (f *fakeStore) ListAlerts(_ context.Context, userID int64, _ int) ([]store.Alert, error) {
	return f.alerts, nil
}

func testServer(t *testing.T) (*Server, *fakeStore, http.Handler) {
	t.Helper()
	fs := newFakeStore()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(fs, auth.NewGitHub("id", "secret", "https://x.test/cb"), log, true)
	return s, fs, s.Routes()
}

// signedIn returns a request carrying a valid session for user 1.
func signedIn(t *testing.T, fs *fakeStore, method, path, body string) *http.Request {
	t.Helper()
	token, hash, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	fs.sessions[key(hash)] = 1

	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: token})
	return r
}

// ── Health ────────────────────────────────────────────────────────────────────

func TestHealthzNeedsNoSession(t *testing.T) {
	_, _, h := testServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// ── Authentication ────────────────────────────────────────────────────────────

func TestEveryAPIRouteRejectsAnAnonymousCaller(t *testing.T) {
	_, _, h := testServer(t)
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/api/me"},
		{http.MethodGet, "/api/rules"},
		{http.MethodPost, "/api/rules"},
		{http.MethodDelete, "/api/rules/1"},
		{http.MethodGet, "/api/alerts"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(route.method, route.path, strings.NewReader("{}")))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401", route.method, route.path, rec.Code)
		}
	}
}

func TestAForgedSessionCookieIsRejected(t *testing.T) {
	_, _, h := testServer(t)
	r := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: "made-up-token"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAValidSessionReachesTheHandler(t *testing.T) {
	_, fs, h := testServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, signedIn(t, fs, http.MethodGet, "/api/me", ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["login"] != "cale" {
		t.Fatalf("login = %v, want cale", body["login"])
	}
}

func TestOnlyTheHashOfTheTokenIsEverStored(t *testing.T) {
	// The cookie carries the raw token; the store must never see it.
	_, fs, h := testServer(t)
	r := signedIn(t, fs, http.MethodGet, "/api/me", "")
	raw := r.Cookies()[0].Value

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	for stored := range fs.sessions {
		if stored == raw {
			t.Fatal("the raw session token was stored")
		}
	}
}

func TestLoginRedirectsToGitHubAndSetsState(t *testing.T) {
	_, _, h := testServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Location"), "github.com/login/oauth/authorize") {
		t.Fatalf("Location = %q", rec.Header().Get("Location"))
	}
	var sawState bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == "fa_oauth_state" && c.Value != "" {
			sawState = true
		}
	}
	if !sawState {
		t.Fatal("the CSRF state cookie was not set")
	}
}

func TestSignInIsUnavailableRatherThanBrokenWhenUnconfigured(t *testing.T) {
	fs := newFakeStore()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(fs, auth.NewGitHub("", "", ""), log, true).Routes()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 with a clear message", rec.Code)
	}
}

func TestACallbackWithoutStateIsRejected(t *testing.T) {
	_, _, h := testServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/callback?code=abc", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestLogoutDeletesTheSessionServerSide(t *testing.T) {
	// Clearing the cookie alone would leave a working token with anyone who had
	// already copied it.
	_, fs, h := testServer(t)
	r := signedIn(t, fs, http.MethodPost, "/auth/logout", "")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(fs.sessions) != 0 {
		t.Fatal("the session survived logout")
	}
}

// ── Rules ─────────────────────────────────────────────────────────────────────

func TestCreatingAValidRule(t *testing.T) {
	_, fs, h := testServer(t)
	body := `{"kind":"operator","target":"vr","threshold_minutes":15}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, signedIn(t, fs, http.MethodPost, "/api/rules", body))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
	}
	var got ruleResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Kind != "operator" || got.Target != "vr" || got.ThresholdMinutes != 15 {
		t.Fatalf("unexpected rule: %+v", got)
	}
}

func TestAnInvalidRuleIsA400NotA500(t *testing.T) {
	_, fs, h := testServer(t)
	for name, body := range map[string]string{
		"unknown kind":   `{"kind":"corridor","target":"x","threshold_minutes":15}`,
		"blank target":   `{"kind":"operator","target":"  ","threshold_minutes":15}`,
		"zero threshold": `{"kind":"operator","target":"vr","threshold_minutes":0}`,
		"absurd window":  `{"kind":"operator","target":"vr","threshold_minutes":100000}`,
		"malformed json": `{"kind":`,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, signedIn(t, fs, http.MethodPost, "/api/rules", body))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, rec.Code)
		}
	}
}

func TestAWebhookPointingAtAPrivateAddressIsRejectedAtCreation(t *testing.T) {
	// Better a 400 the user sees than a failure buried in a poll log.
	_, fs, h := testServer(t)
	body := `{"kind":"operator","target":"vr","threshold_minutes":15,
	          "webhook_url":"http://169.254.169.254/latest/meta-data"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, signedIn(t, fs, http.MethodPost, "/api/rules", body))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "private address") {
		t.Fatalf("body = %s", rec.Body)
	}
}

func TestListRulesReturnsOnlyTheCallersRules(t *testing.T) {
	_, fs, h := testServer(t)
	fs.rules[10] = rules.Rule{ID: 10, UserID: 1, Kind: rules.KindOperator, Target: "vr", ThresholdMinutes: 15, Active: true}
	fs.rules[11] = rules.Rule{ID: 11, UserID: 2, Kind: rules.KindOperator, Target: "vr", ThresholdMinutes: 15, Active: true}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, signedIn(t, fs, http.MethodGet, "/api/rules", ""))

	var got struct {
		Rules []ruleResponse `json:"rules"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.Rules) != 1 || got.Rules[0].ID != 10 {
		t.Fatalf("got %+v, want only rule 10", got.Rules)
	}
}

func TestDeletingAnotherUsersRuleIs404(t *testing.T) {
	_, fs, h := testServer(t)
	fs.rules[11] = rules.Rule{ID: 11, UserID: 2, Kind: rules.KindOperator, Target: "vr", ThresholdMinutes: 15}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, signedIn(t, fs, http.MethodDelete, "/api/rules/11", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if _, still := fs.rules[11]; !still {
		t.Fatal("another user's rule was deleted")
	}
}

func TestDeletingOwnRuleReturns204(t *testing.T) {
	_, fs, h := testServer(t)
	fs.rules[10] = rules.Rule{ID: 10, UserID: 1, Kind: rules.KindOperator, Target: "vr", ThresholdMinutes: 15}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, signedIn(t, fs, http.MethodDelete, "/api/rules/10", ""))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
}

func TestANonNumericRuleIDIs400(t *testing.T) {
	_, fs, h := testServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, signedIn(t, fs, http.MethodDelete, "/api/rules/abc", ""))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestAStoreFailureIsA500WithoutLeakingDetail(t *testing.T) {
	_, fs, h := testServer(t)
	fs.failOn = "CreateRule"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, signedIn(t, fs, http.MethodPost, "/api/rules",
		`{"kind":"operator","target":"vr","threshold_minutes":15}`))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Fatal("the internal error leaked to the client")
	}
}

// ── Alerts ────────────────────────────────────────────────────────────────────

func TestAlertsRenderWithISODates(t *testing.T) {
	_, fs, h := testServer(t)
	fs.alerts = []store.Alert{{
		ID: 5, RuleID: 10, TrainNumber: 3001,
		DepartureDate: time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC),
		Operator:      "vr", DelayMinutes: 22, Station: "Tampere",
		CreatedAt: time.Date(2026, 8, 16, 12, 30, 0, 0, time.UTC),
	}}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, signedIn(t, fs, http.MethodGet, "/api/alerts", ""))

	var got struct {
		Alerts []alertResponse `json:"alerts"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.Alerts) != 1 {
		t.Fatalf("got %d alerts, want 1", len(got.Alerts))
	}
	if got.Alerts[0].DepartureDate != "2026-08-16" {
		t.Errorf("departure_date = %q", got.Alerts[0].DepartureDate)
	}
	if got.Alerts[0].CreatedAt != "2026-08-16T12:30:00Z" {
		t.Errorf("created_at = %q, want RFC3339", got.Alerts[0].CreatedAt)
	}
}

func TestAnEmptyAlertFeedIsAnArrayNotNull(t *testing.T) {
	// `null` forces every client to special-case it; `[]` does not.
	_, fs, h := testServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, signedIn(t, fs, http.MethodGet, "/api/alerts", ""))
	if !strings.Contains(rec.Body.String(), `"alerts":[]`) {
		t.Fatalf("body = %s, want an empty array", rec.Body)
	}
}

func TestTheAlertFeedCarriesDataAttribution(t *testing.T) {
	// The train data is CC BY 4.0, and alerts are a derived work from it, so the
	// credit has to travel with the API response rather than live only in the
	// README where an API consumer would never see it.
	_, fs, h := testServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, signedIn(t, fs, http.MethodGet, "/api/alerts", ""))

	body := rec.Body.String()
	for _, want := range []string{"Fintraffic", "CC BY 4.0", "digitraffic.fi"} {
		if !strings.Contains(body, want) {
			t.Errorf("attribution missing %q from %s", want, body)
		}
	}
}

// ── Device tokens (native clients) ────────────────────────────────────────────

// bearer returns a request authenticated with a bearer token instead of a cookie.
func bearer(t *testing.T, fs *fakeStore, method, path, body string) *http.Request {
	t.Helper()
	token, hash, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	fs.sessions[key(hash)] = 1

	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

func TestABearerTokenAuthenticatesLikeACookie(t *testing.T) {
	// A native client has no cookie jar and no browser to redirect, so the same
	// credential has to work as a bearer.
	_, fs, h := testServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, bearer(t, fs, http.MethodGet, "/api/me", ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
}

func TestABogusBearerTokenIsRejected(t *testing.T) {
	_, _, h := testServer(t)
	r := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	r.Header.Set("Authorization", "Bearer not-a-real-token")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestANonBearerAuthorizationHeaderIsIgnored(t *testing.T) {
	_, _, h := testServer(t)
	r := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	r.Header.Set("Authorization", "Basic dXNlcjpwYXNz")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 — Basic auth is not a scheme this API accepts", rec.Code)
	}
}

func TestIssuingADeviceTokenReturnsItOnce(t *testing.T) {
	_, fs, h := testServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, signedIn(t, fs, http.MethodPost, "/api/tokens", ""))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
	}
	var got struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Token == "" || got.ExpiresAt == "" {
		t.Fatalf("response = %s, want a token and an expiry", rec.Body)
	}
	// Only the hash is retained, which is why it can never be shown again.
	for stored := range fs.sessions {
		if stored == got.Token {
			t.Fatal("the plaintext device token was stored")
		}
	}
}

func TestAnIssuedDeviceTokenActuallyWorks(t *testing.T) {
	_, fs, h := testServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, signedIn(t, fs, http.MethodPost, "/api/tokens", ""))

	var got struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)

	r := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	r.Header.Set("Authorization", "Bearer "+got.Token)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, r)

	if rec2.Code != http.StatusOK {
		t.Fatalf("the issued token did not authenticate: %d %s", rec2.Code, rec2.Body)
	}
}

func TestIssuingATokenRequiresBeingSignedIn(t *testing.T) {
	_, _, h := testServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/tokens", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
