// Package store is the Postgres persistence layer.
//
// The one thing worth reading closely is InsertAlerts. Suppression of duplicate
// alerts is not implemented here in Go -- it is a UNIQUE constraint in the
// schema, and this package only counts what the database let through. That is
// deliberate: the poller can overlap with itself, and a read-then-write check in
// application code would let two runs both decide an alert is new.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/calemccammon/freight-alerts/internal/rules"
)

var ErrNotFound = errors.New("not found")

type Store struct{ pool *pgxpool.Pool }

func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

// Pool exposes the connection pool for the migrator.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// ── Users and sessions ────────────────────────────────────────────────────────

type User struct {
	ID       int64
	GitHubID int64
	Login    string
}

// UpsertUser creates the user on first sign-in and refreshes the login after,
// since a GitHub username can change while the numeric id cannot.
func (s *Store) UpsertUser(ctx context.Context, githubID int64, login string) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx, `
		INSERT INTO users (github_id, login) VALUES ($1, $2)
		ON CONFLICT (github_id) DO UPDATE SET login = EXCLUDED.login
		RETURNING id, github_id, login`, githubID, login).Scan(&u.ID, &u.GitHubID, &u.Login)
	if err != nil {
		return User{}, fmt.Errorf("upsert user: %w", err)
	}
	return u, nil
}

func (s *Store) CreateSession(ctx context.Context, userID int64, tokenHash []byte, expiresAt time.Time) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`,
		tokenHash, userID, expiresAt)
	return err
}

// UserBySession resolves a session token hash, treating an expired session as
// absent so callers cannot accidentally accept one.
func (s *Store) UserBySession(ctx context.Context, tokenHash []byte) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx, `
		SELECT u.id, u.github_id, u.login
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.expires_at > now()`, tokenHash).
		Scan(&u.ID, &u.GitHubID, &u.Login)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash []byte) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, tokenHash)
	return err
}

// PurgeExpiredSessions keeps the table from growing without bound; the poller
// calls it, so there is no separate cleanup job to forget about.
func (s *Store) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at <= now()`)
	return tag.RowsAffected(), err
}

// ── Rules ─────────────────────────────────────────────────────────────────────

func (s *Store) CreateRule(ctx context.Context, r rules.Rule) (rules.Rule, error) {
	if err := r.Validate(); err != nil {
		return rules.Rule{}, err
	}
	// Re-submitting an identical rule returns the existing one rather than
	// creating a second subscription that would double every future alert.
	err := s.pool.QueryRow(ctx, `
		INSERT INTO watch_rules (user_id, kind, target, threshold_minutes, webhook_url)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (user_id, kind, target, threshold_minutes)
		DO UPDATE SET active = TRUE, webhook_url = EXCLUDED.webhook_url
		RETURNING id, active`,
		r.UserID, string(r.Kind), r.Target, r.ThresholdMinutes, r.WebhookURL).
		Scan(&r.ID, &r.Active)
	if err != nil {
		return rules.Rule{}, fmt.Errorf("create rule: %w", err)
	}
	return r, nil
}

func (s *Store) ListRules(ctx context.Context, userID int64) ([]rules.Rule, error) {
	rowsQ, err := s.pool.Query(ctx, `
		SELECT id, user_id, kind, target, threshold_minutes, webhook_url, active
		FROM watch_rules WHERE user_id = $1 ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	return scanRules(rowsQ)
}

// ActiveRules returns every active rule across all users -- what the poller
// evaluates. Rules are few and polls are infrequent, so this stays a single
// query rather than a per-user fan-out.
func (s *Store) ActiveRules(ctx context.Context) ([]rules.Rule, error) {
	rowsQ, err := s.pool.Query(ctx, `
		SELECT id, user_id, kind, target, threshold_minutes, webhook_url, active
		FROM watch_rules WHERE active ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return scanRules(rowsQ)
}

func scanRules(rowsQ pgx.Rows) ([]rules.Rule, error) {
	defer rowsQ.Close()
	var out []rules.Rule
	for rowsQ.Next() {
		var r rules.Rule
		var kind string
		if err := rowsQ.Scan(&r.ID, &r.UserID, &kind, &r.Target,
			&r.ThresholdMinutes, &r.WebhookURL, &r.Active); err != nil {
			return nil, err
		}
		r.Kind = rules.Kind(kind)
		out = append(out, r)
	}
	return out, rowsQ.Err()
}

// DeleteRule scopes the delete by user so an id from another account cannot be
// removed by guessing it.
func (s *Store) DeleteRule(ctx context.Context, userID, ruleID int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM watch_rules WHERE id = $1 AND user_id = $2`, ruleID, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ── Alerts ────────────────────────────────────────────────────────────────────

type Alert struct {
	ID            int64
	RuleID        int64
	TrainNumber   int
	DepartureDate time.Time
	Operator      string
	DelayMinutes  int
	Station       string
	CreatedAt     time.Time
}

// InsertAlerts persists the alerts that are genuinely new and returns them.
//
// The duplicate check is the UNIQUE (rule_id, train_number, departure_date)
// constraint, not a query this code runs first. ON CONFLICT DO NOTHING means an
// overlapping poll silently loses the race instead of producing a second
// notification, and RETURNING tells us which rows actually landed -- so only
// those get a webhook.
func (s *Store) InsertAlerts(ctx context.Context, candidates []rules.Alert) ([]rules.Alert, error) {
	var created []rules.Alert
	for _, a := range candidates {
		departure, err := time.Parse("2006-01-02", a.DepartureDate)
		if err != nil {
			return nil, fmt.Errorf("alert for train %d has an unparseable departure date %q: %w",
				a.TrainNumber, a.DepartureDate, err)
		}
		var id int64
		err = s.pool.QueryRow(ctx, `
			INSERT INTO alerts (rule_id, user_id, train_number, departure_date,
			                    operator, delay_minutes, station)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (rule_id, train_number, departure_date) DO NOTHING
			RETURNING id`,
			a.RuleID, a.UserID, a.TrainNumber, departure,
			a.Operator, a.DelayMinutes, a.Station).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			continue // already alerted for this rule/train/day
		}
		if err != nil {
			return nil, fmt.Errorf("insert alert: %w", err)
		}
		created = append(created, a)
	}
	return created, nil
}

func (s *Store) ListAlerts(ctx context.Context, userID int64, limit int) ([]Alert, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rowsQ, err := s.pool.Query(ctx, `
		SELECT id, rule_id, train_number, departure_date, operator,
		       delay_minutes, station, created_at
		FROM alerts WHERE user_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2`,
		userID, limit)
	if err != nil {
		return nil, err
	}
	defer rowsQ.Close()

	var out []Alert
	for rowsQ.Next() {
		var a Alert
		if err := rowsQ.Scan(&a.ID, &a.RuleID, &a.TrainNumber, &a.DepartureDate,
			&a.Operator, &a.DelayMinutes, &a.Station, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rowsQ.Err()
}

func (s *Store) MarkWebhookStatus(ctx context.Context, ruleID int64, trainNumber int, departureDate, status string) error {
	departure, err := time.Parse("2006-01-02", departureDate)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		UPDATE alerts SET webhook_status = $4
		WHERE rule_id = $1 AND train_number = $2 AND departure_date = $3`,
		ruleID, trainNumber, departure, status)
	return err
}

// ── Poll runs ─────────────────────────────────────────────────────────────────

func (s *Store) StartPollRun(ctx context.Context) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `INSERT INTO poll_runs DEFAULT VALUES RETURNING id`).Scan(&id)
	return id, err
}

func (s *Store) FinishPollRun(ctx context.Context, id int64, trains, rulesEvaluated, alertsCreated int, runErr error) error {
	var msg *string
	if runErr != nil {
		text := runErr.Error()
		msg = &text
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE poll_runs
		SET finished_at = now(), trains_seen = $2, rules_evaluated = $3,
		    alerts_created = $4, error = $5
		WHERE id = $1`, id, trains, rulesEvaluated, alertsCreated, msg)
	return err
}
