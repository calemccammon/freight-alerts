package api

import (
	"net/http"
	"strings"
)

// Origins is the set of browser origins permitted to call this API
// cross-origin.
//
// Unlike Allowlist, the default here is *closed*. An unset allowlist only
// widens who may sign in, and is warned about at startup; an unset CORS policy
// that defaulted to open would let any page on the internet make authenticated
// requests from a visitor's browser. Nothing needs CORS unless a browser client
// is served from another origin, so silence means no cross-origin access.
type Origins struct {
	allowed map[string]struct{}
}

// ParseOrigins reads a comma-separated list of origins, each a scheme and host
// with no trailing slash -- "https://calemccammon.github.io", not
// "https://calemccammon.github.io/". A browser sends the former in the Origin
// header, so a trailing slash here would silently never match.
func ParseOrigins(raw string) Origins {
	allowed := make(map[string]struct{})
	for _, field := range strings.Split(raw, ",") {
		origin := strings.TrimRight(strings.TrimSpace(field), "/")
		if origin != "" {
			allowed[origin] = struct{}{}
		}
	}
	return Origins{allowed: allowed}
}

// Permits reports whether origin may call the API from a browser.
func (o Origins) Permits(origin string) bool {
	_, ok := o.allowed[origin]
	return ok
}

// Size reports how many origins are permitted.
func (o Origins) Size() int { return len(o.allowed) }

// withCORS answers preflight requests and adds the response headers a browser
// needs before it will hand a cross-origin response to page scripts.
//
// Two deliberate omissions:
//
//   - The permitted origin is echoed back, never "*". A wildcard would be
//     simpler, but it also means any site can read responses, and the value is
//     what a browser checks against the page's own origin.
//   - Access-Control-Allow-Credentials is never set, so browsers will not send
//     the session cookie cross-origin. Cross-origin clients authenticate with a
//     bearer token instead, which is what the Flutter app does. This is why a
//     misconfigured origin cannot turn into a cross-site request forgery
//     against a signed-in browser session: there is no ambient credential to
//     forge with.
func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && s.origins.Permits(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			// Without this, a cache could serve one origin's response to
			// another, since the body is identical but the header is not.
			w.Header().Add("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Max-Age", "600")
		}

		// Preflight is answered here rather than reaching the mux, which has no
		// OPTIONS routes and would return 405 -- the failure this middleware
		// exists to fix.
		if r.Method == http.MethodOptions && origin != "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}
