package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calemccammon/freight-alerts/internal/auth"
)

func TestAnUnsetAllowlistPermitsAnyone(t *testing.T) {
	// The default must stay open so that cloning this repository yields a
	// working service without any configuration.
	a := ParseAllowlist("")
	if !a.Open() {
		t.Fatal("an unset allowlist should be open")
	}
	if !a.Permits("anybody") {
		t.Error("an open allowlist should permit any login")
	}
}

func TestAConfiguredAllowlistPermitsOnlyItsLogins(t *testing.T) {
	a := ParseAllowlist("calemccammon,octocat")
	if a.Open() {
		t.Fatal("a configured allowlist should not report itself open")
	}
	for _, login := range []string{"calemccammon", "octocat"} {
		if !a.Permits(login) {
			t.Errorf("%q should be permitted", login)
		}
	}
	if a.Permits("stranger") {
		t.Error("a login outside the list should be refused")
	}
}

func TestAllowlistComparisonIsCaseInsensitive(t *testing.T) {
	// GitHub logins are case-insensitive, so signing in as "CalEmcCammon"
	// must not slip past an entry written in lower case.
	a := ParseAllowlist("calemccammon")
	if !a.Permits("CalEmcCammon") {
		t.Error("differently-cased login should still be permitted")
	}
	if !ParseAllowlist("OctoCat").Permits("octocat") {
		t.Error("case in the configured value should not matter either")
	}
}

func TestAllowlistIgnoresWhitespaceAndBlankFields(t *testing.T) {
	a := ParseAllowlist(" calemccammon , , octocat ,")
	if got := a.Size(); got != 2 {
		t.Fatalf("size = %d, want 2", got)
	}
	if !a.Permits("octocat") {
		t.Error("a padded entry should still be permitted")
	}
}

func TestAValueWithNoUsableEntriesIsOpenRatherThanClosed(t *testing.T) {
	// Permitting nobody would lock an operator out of their own deployment
	// over a stray comma, with a 403 indistinguishable from a correct refusal.
	// Serve() warns at startup so this cannot widen access unnoticed.
	if !ParseAllowlist(" , ,").Open() {
		t.Error("a value with no usable entries should be treated as unset")
	}
}

// fakeOAuth resolves to a fixed identity. The api package depends on three
// methods of the sign-in provider, so a fake covers the whole callback path
// without an HTTP stub -- and without the real client needing a way to be
// pointed somewhere else. The genuine token exchange is tested in the auth
// package, against its own server, where it belongs.
type fakeOAuth struct{ login string }

func (fakeOAuth) Configured() bool { return true }

func (fakeOAuth) AuthorizeURL(state string) string {
	return "https://github.test/login/oauth/authorize?state=" + state
}

func (f fakeOAuth) Exchange(context.Context, string) (auth.Identity, error) {
	return auth.Identity{ID: 4242, Login: f.login}, nil
}

// callbackAs runs a complete OAuth callback for login against an allowlist.
func callbackAs(t *testing.T, allow Allowlist, login string) (*httptest.ResponseRecorder, *fakeStore) {
	t.Helper()
	fs := newFakeStore()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(fs, fakeOAuth{login: login}, log, false, allow).Routes()

	// The callback verifies state against a cookie, so mint a matching pair.
	stateRec := httptest.NewRecorder()
	state, err := auth.NewState()
	if err != nil {
		t.Fatal(err)
	}
	auth.SetStateCookie(stateRec, state, false)

	r := httptest.NewRequest(http.MethodGet, "/auth/callback?code=abc&state="+state, nil)
	for _, c := range stateRec.Result().Cookies() {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w, fs
}

func TestAPermittedLoginCompletesSignIn(t *testing.T) {
	w, fs := callbackAs(t, ParseAllowlist("octocat"), "octocat")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	if len(fs.sessions) != 1 {
		t.Errorf("sessions created = %d, want 1", len(fs.sessions))
	}
}

func TestARefusedLoginGetsA403AndCreatesNoUser(t *testing.T) {
	// The refusal must land before UpsertUser, or every rejected sign-in would
	// still leave an account row behind.
	w, fs := callbackAs(t, ParseAllowlist("octocat"), "stranger")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	// The fake seeds one user and UpsertUser overwrites that row, so an
	// untouched login is what proves the write never happened; a count would
	// stay at one either way.
	if got := fs.users[1].Login; got != "cale" {
		t.Errorf("user row was written as %q; refusal must precede UpsertUser", got)
	}
	if len(fs.sessions) != 0 {
		t.Errorf("sessions created = %d, want 0", len(fs.sessions))
	}
}

func TestARefusalExplainsItselfRatherThanJustDenying(t *testing.T) {
	// A browser lands here by redirect; a bare 403 would read as a broken
	// deployment rather than a deliberate policy.
	w, _ := callbackAs(t, ParseAllowlist("octocat"), "stranger")
	body := w.Body.String()
	if !strings.Contains(body, "limited to its owner") {
		t.Errorf("refusal should say the instance is restricted, got %s", body)
	}
	if !strings.Contains(body, "github.com/calemccammon/freight-alerts") {
		t.Errorf("refusal should point at the source, got %s", body)
	}
}

func TestAnOpenAllowlistStillLetsAnyoneSignIn(t *testing.T) {
	// Guards the clone-and-run default against a regression that would make
	// an unset variable deny everybody.
	w, fs := callbackAs(t, ParseAllowlist(""), "somebody-new")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	if len(fs.sessions) != 1 {
		t.Errorf("sessions created = %d, want 1", len(fs.sessions))
	}
}
