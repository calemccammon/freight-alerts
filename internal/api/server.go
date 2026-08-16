// Package api is the HTTP surface: GitHub sign-in, rule CRUD, and the alert feed.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/calemccammon/freight-alerts/internal/auth"
	"github.com/calemccammon/freight-alerts/internal/rules"
	"github.com/calemccammon/freight-alerts/internal/store"
	"github.com/calemccammon/freight-alerts/internal/webhook"
)

// Store is declared here, at the consumer, rather than in the store package --
// the Go convention, and it means the handler tests run against a fake with no
// database at all.
type Store interface {
	UpsertUser(ctx context.Context, githubID int64, login string) (store.User, error)
	CreateSession(ctx context.Context, userID int64, hash []byte, expires time.Time) error
	UserBySession(ctx context.Context, hash []byte) (store.User, error)
	DeleteSession(ctx context.Context, hash []byte) error
	CreateRule(ctx context.Context, r rules.Rule) (rules.Rule, error)
	ListRules(ctx context.Context, userID int64) ([]rules.Rule, error)
	DeleteRule(ctx context.Context, userID, ruleID int64) error
	ListAlerts(ctx context.Context, userID int64, limit int) ([]store.Alert, error)
}

type Server struct {
	store  Store
	github *auth.GitHub
	log    *slog.Logger
	// secure controls the cookie Secure flag; off only when running locally
	// over plain http.
	secure bool
}

func New(s Store, gh *auth.GitHub, log *slog.Logger, secure bool) *Server {
	return &Server{store: s, github: gh, log: log, secure: secure}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /auth/login", s.login)
	mux.HandleFunc("GET /auth/callback", s.callback)
	mux.HandleFunc("POST /auth/logout", s.logout)

	mux.Handle("GET /api/me", s.requireUser(s.me))
	mux.Handle("GET /api/rules", s.requireUser(s.listRules))
	mux.Handle("POST /api/rules", s.requireUser(s.createRule))
	mux.Handle("DELETE /api/rules/{id}", s.requireUser(s.deleteRule))
	mux.Handle("GET /api/alerts", s.requireUser(s.listAlerts))

	return mux
}

// ── Plumbing ──────────────────────────────────────────────────────────────────

type ctxKey int

const userKey ctxKey = iota

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// writeError keeps the wire shape uniform so a client never has to guess
// whether a failure is JSON or a bare string.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// requireUser resolves the session cookie and rejects anything without one.
//
// The cookie holds the raw token; the database holds only its hash, so the
// lookup hashes first and a stolen database cannot be replayed as a session.
func (s *Server) requireUser(next func(http.ResponseWriter, *http.Request, store.User)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(auth.SessionCookie)
		if err != nil || cookie.Value == "" {
			writeError(w, http.StatusUnauthorized, "sign in at /auth/login")
			return
		}
		user, err := s.store.UserBySession(r.Context(), auth.HashToken(cookie.Value))
		if err != nil {
			// An expired session is indistinguishable from a forged one here,
			// which is the point: both are simply not signed in.
			auth.ClearSessionCookie(w, s.secure)
			writeError(w, http.StatusUnauthorized, "session expired; sign in again")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userKey, user)), user)
	})
}

// ── Handlers ──────────────────────────────────────────────────────────────────

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.github.Configured() {
		writeError(w, http.StatusServiceUnavailable, "GitHub sign-in is not configured on this deployment")
		return
	}
	state, err := auth.NewState()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start sign-in")
		return
	}
	auth.SetStateCookie(w, state, s.secure)
	http.Redirect(w, r, s.github.AuthorizeURL(state), http.StatusFound)
}

func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	if !s.github.Configured() {
		writeError(w, http.StatusServiceUnavailable, "GitHub sign-in is not configured")
		return
	}
	// Verified before the code is spent, so a callback the user never initiated
	// cannot be replayed against them.
	if err := auth.VerifyState(w, r, r.URL.Query().Get("state"), s.secure); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		writeError(w, http.StatusBadRequest, "missing authorization code")
		return
	}

	identity, err := s.github.Exchange(r.Context(), code)
	if err != nil {
		s.log.Warn("oauth exchange failed", "err", err)
		writeError(w, http.StatusBadGateway, "GitHub sign-in failed")
		return
	}

	user, err := s.store.UpsertUser(r.Context(), identity.ID, identity.Login)
	if err != nil {
		s.log.Error("upsert user", "err", err)
		writeError(w, http.StatusInternalServerError, "could not complete sign-in")
		return
	}

	token, hash, err := auth.NewToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create session")
		return
	}
	if err := s.store.CreateSession(r.Context(), user.ID, hash, time.Now().Add(auth.SessionTTL)); err != nil {
		s.log.Error("create session", "err", err)
		writeError(w, http.StatusInternalServerError, "could not create session")
		return
	}
	auth.SetSessionCookie(w, token, s.secure)
	writeJSON(w, http.StatusOK, map[string]any{"login": user.Login, "id": user.ID})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(auth.SessionCookie); err == nil && cookie.Value != "" {
		// Deleted server-side too: clearing the cookie alone would leave a
		// working token in anyone's hands who had already copied it.
		if err := s.store.DeleteSession(r.Context(), auth.HashToken(cookie.Value)); err != nil {
			s.log.Warn("delete session", "err", err)
		}
	}
	auth.ClearSessionCookie(w, s.secure)
	writeJSON(w, http.StatusOK, map[string]string{"status": "signed out"})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request, u store.User) {
	writeJSON(w, http.StatusOK, map[string]any{"id": u.ID, "login": u.Login})
}

type ruleRequest struct {
	Kind             string `json:"kind"`
	Target           string `json:"target"`
	ThresholdMinutes int    `json:"threshold_minutes"`
	WebhookURL       string `json:"webhook_url"`
}

type ruleResponse struct {
	ID               int64  `json:"id"`
	Kind             string `json:"kind"`
	Target           string `json:"target"`
	ThresholdMinutes int    `json:"threshold_minutes"`
	WebhookURL       string `json:"webhook_url,omitempty"`
	Active           bool   `json:"active"`
}

func toRuleResponse(r rules.Rule) ruleResponse {
	return ruleResponse{ID: r.ID, Kind: string(r.Kind), Target: r.Target,
		ThresholdMinutes: r.ThresholdMinutes, WebhookURL: r.WebhookURL, Active: r.Active}
}

func (s *Server) createRule(w http.ResponseWriter, r *http.Request, u store.User) {
	var req ruleRequest
	// Bounded so a large body cannot be used to exhaust memory.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	// Validated here rather than only at send time, so a bad URL is a 400 the
	// user sees instead of a failure buried in a poll log.
	if err := webhook.Validate(req.WebhookURL); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	rule := rules.Rule{
		UserID: u.ID, Kind: rules.Kind(req.Kind), Target: req.Target,
		ThresholdMinutes: req.ThresholdMinutes, WebhookURL: req.WebhookURL, Active: true,
	}
	if err := rule.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	created, err := s.store.CreateRule(r.Context(), rule)
	if err != nil {
		s.log.Error("create rule", "err", err)
		writeError(w, http.StatusInternalServerError, "could not save rule")
		return
	}
	writeJSON(w, http.StatusCreated, toRuleResponse(created))
}

func (s *Server) listRules(w http.ResponseWriter, r *http.Request, u store.User) {
	list, err := s.store.ListRules(r.Context(), u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load rules")
		return
	}
	out := make([]ruleResponse, 0, len(list))
	for _, rule := range list {
		out = append(out, toRuleResponse(rule))
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": out})
}

func (s *Server) deleteRule(w http.ResponseWriter, r *http.Request, u store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "rule id must be a number")
		return
	}
	// The store scopes the delete by user, so another account's id is a 404
	// rather than a deletion.
	switch err := s.store.DeleteRule(r.Context(), u.ID, id); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such rule")
	default:
		s.log.Error("delete rule", "err", err)
		writeError(w, http.StatusInternalServerError, "could not delete rule")
	}
}

// dataAttribution travels with the alerts because they are a derived work from
// CC BY 4.0 licensed data: any client of this API carries the credit too,
// rather than the obligation living only in this repo's README.
const dataAttribution = "Train data from Fintraffic Digitraffic (https://www.digitraffic.fi/en/), " +
	"licensed under CC BY 4.0 (https://creativecommons.org/licenses/by/4.0/). " +
	"Delays are derived from the most recently reached timetable stop."

type alertResponse struct {
	ID            int64  `json:"id"`
	RuleID        int64  `json:"rule_id"`
	TrainNumber   int    `json:"train_number"`
	DepartureDate string `json:"departure_date"`
	Operator      string `json:"operator"`
	DelayMinutes  int    `json:"delay_minutes"`
	Station       string `json:"station"`
	CreatedAt     string `json:"created_at"`
}

func (s *Server) listAlerts(w http.ResponseWriter, r *http.Request, u store.User) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := s.store.ListAlerts(r.Context(), u.ID, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load alerts")
		return
	}
	out := make([]alertResponse, 0, len(list))
	for _, a := range list {
		out = append(out, alertResponse{
			ID: a.ID, RuleID: a.RuleID, TrainNumber: a.TrainNumber,
			DepartureDate: a.DepartureDate.Format("2006-01-02"),
			Operator:      a.Operator, DelayMinutes: a.DelayMinutes, Station: a.Station,
			CreatedAt: a.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"alerts":      out,
		"attribution": dataAttribution,
	})
}
