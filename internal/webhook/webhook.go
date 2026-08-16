// Package webhook delivers alerts to a URL the user supplied.
//
// That sentence is the whole security problem. A user-controlled URL that the
// server fetches is server-side request forgery: left unguarded it turns this
// service into a proxy for reaching things only the server can reach -- cloud
// metadata endpoints, databases on the private network, admin panels on
// localhost. Everything here exists to stop that.
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"
)

const (
	timeout     = 8 * time.Second
	maxBodyEcho = 200
)

// Validate rejects a webhook URL at the point a rule is created, so a bad one
// is a 400 the user sees rather than a failure buried in a poll log.
func Validate(raw string) error {
	if raw == "" {
		return nil // webhooks are optional
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("webhook_url is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		// file://, gopher://, and friends are how SSRF gets interesting.
		return fmt.Errorf("webhook_url must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("webhook_url must include a host")
	}
	if AllowPrivateAddresses {
		return nil // tests only; see the declaration
	}
	// A literal private address is rejected outright. A hostname that *resolves*
	// to one is caught later, at dial time -- see the Control hook in New.
	if host, _, err := net.SplitHostPort(u.Host); err == nil {
		if ip := net.ParseIP(host); ip != nil && !publiclyRoutable(ip) {
			return fmt.Errorf("webhook_url must not point at a private address")
		}
	} else if ip := net.ParseIP(u.Host); ip != nil && !publiclyRoutable(ip) {
		return fmt.Errorf("webhook_url must not point at a private address")
	}
	return nil
}

// publiclyRoutable reports whether an address is one the open internet can
// reach. Everything else is somewhere only this server can go, which is exactly
// what SSRF is for.
func publiclyRoutable(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	// 169.254.169.254 is link-local and already covered above, but IPv6 unique
	// local addresses (fc00::/7) are not caught by IsPrivate on every Go
	// version, so check explicitly.
	if len(ip) == net.IPv6len && ip.To4() == nil && (ip[0]&0xfe) == 0xfc {
		return false
	}
	return true
}

// Sender posts alerts to user-supplied URLs.
//
// There is deliberately only one constructor. An unguarded variant sitting next
// to a guarded one is a footgun: the wrong import is an SSRF hole, and nothing
// in review would flag it.
type Sender struct{ http *http.Client }

// AllowPrivateAddresses is only ever set by tests, which must dial a local
// httptest server. It is a package-level switch rather than a constructor
// argument so that no production call site can pass it by accident.
var AllowPrivateAddresses = false

func New() *Sender {
	dialer := &net.Dialer{
		Timeout: 5 * time.Second,
		// Control runs after DNS resolution, with the address actually about to
		// be dialled. That closes the gap Validate cannot: a hostname that
		// passes validation and then resolves to a private address -- whether by
		// DNS rebinding or just a record that changed -- is refused here.
		Control: func(_, address string, _ syscall.RawConn) error {
			if AllowPrivateAddresses {
				return nil
			}
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("unexpected dial address %q", address)
			}
			ip := net.ParseIP(host)
			if ip == nil {
				return fmt.Errorf("could not parse dial address %q", host)
			}
			if !publiclyRoutable(ip) {
				return fmt.Errorf("refusing to connect to non-public address %s", ip)
			}
			return nil
		},
	}
	return &Sender{http: &http.Client{
		Timeout: timeout,
		// Redirects are not followed. A permitted public URL that 302s to
		// 169.254.169.254 would otherwise walk straight past every check above.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{DialContext: dialer.DialContext},
	}}
}

// Payload is what a subscriber receives.
type Payload struct {
	TrainNumber   int    `json:"train_number"`
	DepartureDate string `json:"departure_date"`
	Operator      string `json:"operator"`
	DelayMinutes  int    `json:"delay_minutes"`
	Station       string `json:"station"`
	RuleID        int64  `json:"rule_id"`
}

// Send posts one alert. The returned string is recorded against the alert, so a
// delivery failure is visible rather than silent.
func (s *Sender) Send(ctx context.Context, rawURL string, p Payload) (status string, err error) {
	if err := Validate(rawURL); err != nil {
		return "rejected", err
	}
	body, err := json.Marshal(p)
	if err != nil {
		return "encode_failed", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return "bad_request", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "freight-alerts/1")

	resp, err := s.http.Do(req)
	if err != nil {
		return "unreachable", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return fmt.Sprintf("ok_%d", resp.StatusCode), nil
	}
	return fmt.Sprintf("http_%d", resp.StatusCode),
		fmt.Errorf("webhook returned %d", resp.StatusCode)
}
