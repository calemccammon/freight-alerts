// Package auth handles GitHub sign-in and session cookies.
//
// No password is ever accepted, stored, or hashed here, because none exists:
// identity is delegated to GitHub entirely. What this package does own is the
// session token, and the rule it follows is that the server keeps only a hash --
// so a leaked database dump cannot be replayed as a set of live logins.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"time"
)

const (
	SessionCookie = "fa_session"
	stateCookie   = "fa_oauth_state"

	SessionTTL = 30 * 24 * time.Hour
	stateTTL   = 10 * time.Minute

	tokenBytes = 32 // 256 bits; far beyond guessing
)

// NewToken returns a fresh session token and the hash to persist.
//
// The raw token goes to the browser and is never written down server-side; the
// hash goes to the database and is useless to an attacker who steals it.
func NewToken() (token string, hash []byte, err error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("generate session token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	return token, HashToken(token), nil
}

// HashToken is SHA-256 with no salt, deliberately.
//
// A salt and a slow KDF defend against offline cracking of low-entropy secrets;
// this token is 256 random bits, so there is nothing to crack. A slow hash here
// would only add latency to every authenticated request.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// EqualTokens compares in constant time so response timing cannot be used to
// recover a token byte by byte.
func EqualTokens(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}

// SetSessionCookie writes the session cookie.
//
// HttpOnly keeps it away from page scripts, so an XSS bug cannot read it.
// SameSite=Lax stops another site's form post from riding the session, while
// still allowing the top-level redirect back from GitHub. Secure is on unless
// running against plain http locally.
func SetSessionCookie(w http.ResponseWriter, token string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(SessionTTL),
		MaxAge:   int(SessionTTL.Seconds()),
	})
}

func ClearSessionCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// NewState returns an OAuth state value, which exists so a callback the user
// did not initiate cannot be replayed against them. It is stored in a
// short-lived cookie and compared on return.
func NewState() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate oauth state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func SetStateCookie(w http.ResponseWriter, state string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookie,
		Value:    state,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(stateTTL.Seconds()),
	})
}

// VerifyState checks the returned state against the cookie and clears it, so a
// state value cannot be replayed a second time.
func VerifyState(w http.ResponseWriter, r *http.Request, returned string, secure bool) error {
	cookie, err := r.Cookie(stateCookie)
	if err != nil {
		return fmt.Errorf("missing oauth state cookie")
	}
	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Value: "", Path: "/", HttpOnly: true,
		Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
	if returned == "" || !EqualTokens([]byte(cookie.Value), []byte(returned)) {
		return fmt.Errorf("oauth state mismatch")
	}
	return nil
}
