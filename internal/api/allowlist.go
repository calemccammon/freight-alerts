package api

import "strings"

// Allowlist decides which GitHub logins may complete sign-in.
//
// The policy lives in configuration rather than in code because this is a
// public repository: a hard-coded list of logins would make the service
// useless to anyone who cloned it. An unset allowlist is therefore open, and
// it is the deployment -- not the source -- that restricts a given instance.
type Allowlist struct {
	// nil means no restriction. This is deliberately distinct from an empty
	// map, which would permit nobody at all; see ParseAllowlist.
	logins map[string]struct{}
}

// ParseAllowlist reads a comma-separated list of GitHub logins.
//
// Blank fields and surrounding whitespace are ignored, so "a, b," is the same
// list as "a,b". Comparison is case-insensitive because GitHub treats logins
// that way -- signing in as "CalEmcCammon" must not slip past an entry of
// "calemccammon".
func ParseAllowlist(raw string) Allowlist {
	logins := make(map[string]struct{})
	for _, field := range strings.Split(raw, ",") {
		if login := strings.ToLower(strings.TrimSpace(field)); login != "" {
			logins[login] = struct{}{}
		}
	}
	// A value with no usable entries is treated the same as an unset one:
	// open. The alternative -- permitting nobody -- would lock an operator out
	// of their own deployment over a stray comma, with a 403 that looks
	// identical to a correctly configured refusal. Serve() logs which mode is
	// active at startup so a typo here is visible immediately rather than
	// silently widening access.
	if len(logins) == 0 {
		return Allowlist{}
	}
	return Allowlist{logins: logins}
}

// Open reports whether any GitHub account may sign in.
func (a Allowlist) Open() bool { return a.logins == nil }

// Size reports how many logins are permitted; zero when open.
func (a Allowlist) Size() int { return len(a.logins) }

// Permits reports whether login may sign in.
func (a Allowlist) Permits(login string) bool {
	if a.Open() {
		return true
	}
	_, ok := a.logins[strings.ToLower(strings.TrimSpace(login))]
	return ok
}
