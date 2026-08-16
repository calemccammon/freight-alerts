package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

const demoOrigin = "https://calemccammon.github.io"

func corsServer(t *testing.T, raw string) http.Handler {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(newFakeStore(), fakeOAuth{login: "cale"}, log, true,
		Allowlist{}, ParseOrigins(raw)).Routes()
}

func TestPreflightFromAPermittedOriginIs204NotAMethodError(t *testing.T) {
	// The mux has no OPTIONS patterns, so without the middleware this is the
	// 405 that silently breaks every browser client on another origin.
	r := httptest.NewRequest(http.MethodOptions, "/api/rules", nil)
	r.Header.Set("Origin", demoOrigin)
	r.Header.Set("Access-Control-Request-Method", "GET")
	w := httptest.NewRecorder()
	corsServer(t, demoOrigin).ServeHTTP(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != demoOrigin {
		t.Errorf("allow-origin = %q, want %q", got, demoOrigin)
	}
	if got := w.Header().Get("Access-Control-Allow-Headers"); got == "" {
		t.Error("preflight must permit the Authorization header or bearer auth cannot work")
	}
}

func TestAnUnknownOriginGetsNoCORSHeaders(t *testing.T) {
	r := httptest.NewRequest(http.MethodOptions, "/api/rules", nil)
	r.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	corsServer(t, demoOrigin).ServeHTTP(w, r)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("allow-origin = %q, want empty for an unlisted origin", got)
	}
}

func TestTheOriginIsEchoedNeverWildcarded(t *testing.T) {
	// "*" would let any site read responses; the browser checks this value
	// against the page's own origin, so it has to be the specific one.
	r := httptest.NewRequest(http.MethodGet, "/health", nil)
	r.Header.Set("Origin", demoOrigin)
	w := httptest.NewRecorder()
	corsServer(t, demoOrigin).ServeHTTP(w, r)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got == "*" {
		t.Fatal("allow-origin must never be a wildcard")
	}
	if got := w.Header().Get("Vary"); got != "Origin" {
		t.Errorf("Vary = %q, want Origin so caches cannot cross origins", got)
	}
}

func TestCredentialsAreNeverAllowedCrossOrigin(t *testing.T) {
	// Without this header browsers refuse to send the session cookie
	// cross-origin, so a mistakenly permitted origin still has no ambient
	// credential to forge requests with. Cross-origin clients use a bearer
	// token instead.
	r := httptest.NewRequest(http.MethodGet, "/api/rules", nil)
	r.Header.Set("Origin", demoOrigin)
	w := httptest.NewRecorder()
	corsServer(t, demoOrigin).ServeHTTP(w, r)

	if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("allow-credentials = %q, want it never set", got)
	}
}

func TestNoOriginConfiguredMeansNoCrossOriginAccess(t *testing.T) {
	// The default is closed: an unset CORS policy that defaulted to open would
	// expose the API to every page on the internet.
	r := httptest.NewRequest(http.MethodOptions, "/api/rules", nil)
	r.Header.Set("Origin", demoOrigin)
	w := httptest.NewRecorder()
	corsServer(t, "").ServeHTTP(w, r)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("allow-origin = %q, want empty when nothing is configured", got)
	}
}

func TestATrailingSlashInConfigurationIsTolerated(t *testing.T) {
	// Browsers send an Origin with no path or trailing slash, so a configured
	// value with one would never match and the failure would look like CORS
	// simply not working.
	o := ParseOrigins("https://calemccammon.github.io/, http://localhost:8080")
	if !o.Permits(demoOrigin) {
		t.Error("a configured origin with a trailing slash should still match")
	}
	if o.Size() != 2 {
		t.Errorf("size = %d, want 2", o.Size())
	}
}

func TestASameOriginRequestIsUnaffected(t *testing.T) {
	// No Origin header at all: a curl or a same-origin fetch must behave
	// exactly as before the middleware existed.
	r := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	corsServer(t, demoOrigin).ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("allow-origin = %q, want empty without an Origin header", got)
	}
}

// Guards against the middleware accidentally becoming an auth bypass: it runs
// outside requireUser, so a bug there could let a preflight-shaped request
// through to a handler.
func TestCORSDoesNotBypassAuthentication(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		r := httptest.NewRequest(method, "/api/rules", nil)
		r.Header.Set("Origin", demoOrigin)
		w := httptest.NewRecorder()
		corsServer(t, demoOrigin).ServeHTTP(w, r)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s /api/rules = %d, want 401 even from a permitted origin", method, w.Code)
		}
	}
}
