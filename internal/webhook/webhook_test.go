package webhook

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ── Validation ────────────────────────────────────────────────────────────────

func TestAnEmptyURLIsAllowedBecauseWebhooksAreOptional(t *testing.T) {
	if err := Validate(""); err != nil {
		t.Fatalf("empty URL rejected: %v", err)
	}
}

func TestPublicHTTPSIsAccepted(t *testing.T) {
	for _, u := range []string{
		"https://example.com/hook",
		"http://example.com:8080/hook",
		"https://hooks.slack.com/services/T/B/X",
	} {
		if err := Validate(u); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", u, err)
		}
	}
}

func TestNonHTTPSchemesAreRejected(t *testing.T) {
	// file:// and gopher:// are how SSRF gets interesting.
	for _, u := range []string{
		"file:///etc/passwd",
		"gopher://example.com/",
		"ftp://example.com/x",
	} {
		if err := Validate(u); err == nil {
			t.Errorf("Validate(%q) = nil, want a scheme rejection", u)
		}
	}
}

func TestLiteralPrivateAddressesAreRejected(t *testing.T) {
	for _, u := range []string{
		"http://127.0.0.1/hook",             // loopback
		"http://localhost:9000/hook",        // resolves to loopback (caught at dial)
		"http://10.0.0.5/hook",              // RFC1918
		"http://192.168.1.10/hook",          // RFC1918
		"http://172.16.0.1/hook",            // RFC1918
		"http://169.254.169.254/latest/api", // cloud metadata
		"http://[::1]:8080/hook",            // IPv6 loopback
		"http://0.0.0.0/hook",               // unspecified
	} {
		err := Validate(u)
		// localhost is a hostname, not a literal, so it is legitimately caught
		// at dial time rather than here.
		if err == nil && !strings.Contains(u, "localhost") {
			t.Errorf("Validate(%q) = nil, want a private-address rejection", u)
		}
	}
}

func TestAMalformedURLIsRejected(t *testing.T) {
	if err := Validate("http://a b c/"); err == nil {
		t.Fatal("expected a malformed URL to be rejected")
	}
}

func TestAURLWithoutAHostIsRejected(t *testing.T) {
	if err := Validate("https:///nohost"); err == nil {
		t.Fatal("expected a missing host to be rejected")
	}
}

// ── Routability ───────────────────────────────────────────────────────────────

func TestPubliclyRoutableClassifiesAddresses(t *testing.T) {
	cases := map[string]bool{
		"8.8.8.8":         true,
		"1.1.1.1":         true,
		"2606:4700::1111": true,
		"127.0.0.1":       false,
		"10.1.2.3":        false,
		"192.168.0.1":     false,
		"172.20.1.1":      false,
		"169.254.169.254": false, // AWS/GCP metadata
		"::1":             false,
		"fd00::1":         false, // IPv6 unique local
		"224.0.0.1":       false, // multicast
	}
	for addr, want := range cases {
		if got := publiclyRoutable(parseIP(t, addr)); got != want {
			t.Errorf("publiclyRoutable(%s) = %v, want %v", addr, got, want)
		}
	}
}

// ── Sending ───────────────────────────────────────────────────────────────────

func TestSendPostsTheAlertAsJSON(t *testing.T) {
	// The dial guard would refuse httptest's loopback address, so tests opt out
	// of it explicitly -- which is also why that switch exists at package level
	// rather than as a constructor argument.
	AllowPrivateAddresses = true
	t.Cleanup(func() { AllowPrivateAddresses = false })

	var got Payload
	var contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	want := Payload{TrainNumber: 3001, DepartureDate: "2026-08-16", Operator: "vr",
		DelayMinutes: 22, Station: "Tampere", RuleID: 9}
	status, err := New().Send(context.Background(), srv.URL, want)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if status != "ok_204" {
		t.Errorf("status = %q, want ok_204", status)
	}
	if contentType != "application/json" {
		t.Errorf("Content-Type = %q", contentType)
	}
	if got != want {
		t.Errorf("payload = %+v, want %+v", got, want)
	}
}

func TestANonSuccessStatusIsReportedNotSwallowed(t *testing.T) {
	AllowPrivateAddresses = true
	t.Cleanup(func() { AllowPrivateAddresses = false })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	status, err := New().Send(context.Background(), srv.URL, Payload{})
	if err == nil {
		t.Fatal("expected a 500 to be reported as an error")
	}
	if status != "http_500" {
		t.Fatalf("status = %q, want http_500", status)
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	// The attack this blocks: a URL that passes validation and then redirects to
	// the cloud metadata endpoint.
	AllowPrivateAddresses = true
	t.Cleanup(func() { AllowPrivateAddresses = false })

	var reachedTarget bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reachedTarget = true
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirector.Close()

	status, _ := New().Send(context.Background(), redirector.URL, Payload{})
	if reachedTarget {
		t.Fatal("the redirect was followed; a public URL could reach a private one")
	}
	if !strings.HasPrefix(status, "http_3") {
		t.Fatalf("status = %q, want the 302 surfaced rather than followed", status)
	}
}

func TestSendRefusesAnInvalidURLWithoutDialing(t *testing.T) {
	status, err := New().Send(context.Background(), "http://169.254.169.254/latest/meta-data", Payload{})
	if err == nil {
		t.Fatal("expected the metadata endpoint to be refused")
	}
	if status != "rejected" {
		t.Fatalf("status = %q, want rejected", status)
	}
}

func TestTheDialGuardBlocksAHostnameThatResolvesPrivately(t *testing.T) {
	// Validate cannot catch this: "localhost" is a hostname, and only the dial
	// hook sees the address it actually resolves to.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the request should never have been dialled")
	}))
	defer srv.Close()

	port := srv.Listener.Addr().(interface{ String() string }).String()
	port = port[strings.LastIndex(port, ":"):]

	_, err := New().Send(context.Background(), "http://localhost"+port+"/hook", Payload{})
	if err == nil {
		t.Fatal("expected the dial guard to refuse a loopback resolution")
	}
}

func parseIP(t *testing.T, s string) net.IP {
	t.Helper()
	ip := net.ParseIP(s)
	if ip == nil {
		t.Fatalf("bad test address %q", s)
	}
	return ip
}
