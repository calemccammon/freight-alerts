package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/calemccammon/freight-alerts/internal/rules"
)

// These run against a real Postgres because the behaviour under test *is* the
// database: the ON CONFLICT suppression, the CHECK constraints, and the
// user-scoped deletes are all enforced by the schema. A fake store would assert
// that the fake works.
//
// CI provides DATABASE_URL from a Postgres service container. Locally they skip.
func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping database integration tests")
	}
	ctx := context.Background()
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := Migrate(ctx, s.Pool()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Each test starts from an empty database; cascades clear the rest.
	if _, err := s.Pool().Exec(ctx, `TRUNCATE users, poll_runs RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func mustUser(t *testing.T, s *Store, githubID int64, login string) User {
	t.Helper()
	u, err := s.UpsertUser(context.Background(), githubID, login)
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	return u
}

func mustRule(t *testing.T, s *Store, userID int64, kind rules.Kind, target string, threshold int) rules.Rule {
	t.Helper()
	r, err := s.CreateRule(context.Background(), rules.Rule{
		UserID: userID, Kind: kind, Target: target, ThresholdMinutes: threshold,
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	return r
}

// ── Migrations ────────────────────────────────────────────────────────────────

func TestMigrateIsIdempotent(t *testing.T) {
	s := testStore(t)
	applied, err := Migrate(context.Background(), s.Pool())
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("re-running migrations applied %v, want nothing", applied)
	}
}

// ── Users and sessions ────────────────────────────────────────────────────────

func TestUpsertUserIsStableOnGitHubIDAndRefreshesLogin(t *testing.T) {
	s := testStore(t)
	first := mustUser(t, s, 4242, "old-name")
	// A GitHub username can change; the numeric id cannot.
	second := mustUser(t, s, 4242, "new-name")
	if first.ID != second.ID {
		t.Fatalf("same github id produced two users: %d and %d", first.ID, second.ID)
	}
	if second.Login != "new-name" {
		t.Fatalf("login = %q, want the refreshed value", second.Login)
	}
}

func TestSessionRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	u := mustUser(t, s, 1, "cale")
	hash := []byte("hash-of-a-token")

	if err := s.CreateSession(ctx, u.ID, hash, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create session: %v", err)
	}
	got, err := s.UserBySession(ctx, hash)
	if err != nil || got.ID != u.ID {
		t.Fatalf("got (%+v, %v), want user %d", got, err, u.ID)
	}
}

func TestAnExpiredSessionResolvesToNothing(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	u := mustUser(t, s, 1, "cale")
	hash := []byte("stale")
	if err := s.CreateSession(ctx, u.ID, hash, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserBySession(ctx, hash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session returned %v, want ErrNotFound", err)
	}
}

func TestPurgeRemovesOnlyExpiredSessions(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	u := mustUser(t, s, 1, "cale")
	_ = s.CreateSession(ctx, u.ID, []byte("live"), time.Now().Add(time.Hour))
	_ = s.CreateSession(ctx, u.ID, []byte("dead"), time.Now().Add(-time.Hour))

	n, err := s.PurgeExpiredSessions(ctx)
	if err != nil || n != 1 {
		t.Fatalf("purged %d (err %v), want 1", n, err)
	}
	if _, err := s.UserBySession(ctx, []byte("live")); err != nil {
		t.Fatalf("purge removed a live session: %v", err)
	}
}

func TestDeletingAUserCascadesToSessions(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	u := mustUser(t, s, 1, "cale")
	_ = s.CreateSession(ctx, u.ID, []byte("tok"), time.Now().Add(time.Hour))

	if _, err := s.Pool().Exec(ctx, `DELETE FROM users WHERE id = $1`, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserBySession(ctx, []byte("tok")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("session survived its user: %v", err)
	}
}

// ── Rules ─────────────────────────────────────────────────────────────────────

func TestCreatingTheSameRuleTwiceDoesNotDuplicateIt(t *testing.T) {
	// Two identical subscriptions would double every future alert.
	s := testStore(t)
	u := mustUser(t, s, 1, "cale")
	first := mustRule(t, s, u.ID, rules.KindOperator, "vr", 15)
	second := mustRule(t, s, u.ID, rules.KindOperator, "vr", 15)
	if first.ID != second.ID {
		t.Fatalf("duplicate rule created: %d and %d", first.ID, second.ID)
	}
	list, err := s.ListRules(context.Background(), u.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("got %d rules (err %v), want 1", len(list), err)
	}
}

func TestADifferentThresholdIsADifferentRule(t *testing.T) {
	s := testStore(t)
	u := mustUser(t, s, 1, "cale")
	a := mustRule(t, s, u.ID, rules.KindOperator, "vr", 15)
	b := mustRule(t, s, u.ID, rules.KindOperator, "vr", 60)
	if a.ID == b.ID {
		t.Fatal("thresholds 15 and 60 should be separate subscriptions")
	}
}

func TestAnInvalidRuleIsRejectedBeforeItReachesTheDatabase(t *testing.T) {
	s := testStore(t)
	u := mustUser(t, s, 1, "cale")
	_, err := s.CreateRule(context.Background(), rules.Rule{
		UserID: u.ID, Kind: rules.KindOperator, Target: "vr", ThresholdMinutes: 0,
	})
	if err == nil {
		t.Fatal("expected a zero threshold to be rejected")
	}
}

func TestTheSchemaAlsoRejectsABadThreshold(t *testing.T) {
	// Belt and braces: validation lives in Go, but the CHECK constraint means a
	// direct write or a future code path cannot bypass it.
	ctx := context.Background()
	s := testStore(t)
	u := mustUser(t, s, 1, "cale")
	_, err := s.Pool().Exec(ctx, `
		INSERT INTO watch_rules (user_id, kind, target, threshold_minutes)
		VALUES ($1, 'operator', 'vr', 0)`, u.ID)
	if err == nil {
		t.Fatal("expected the CHECK constraint to reject threshold 0")
	}
}

func TestActiveRulesSpansUsers(t *testing.T) {
	s := testStore(t)
	a := mustUser(t, s, 1, "a")
	b := mustUser(t, s, 2, "b")
	mustRule(t, s, a.ID, rules.KindOperator, "vr", 15)
	mustRule(t, s, b.ID, rules.KindTrain, "3001", 10)

	all, err := s.ActiveRules(context.Background())
	if err != nil || len(all) != 2 {
		t.Fatalf("got %d active rules (err %v), want 2", len(all), err)
	}
}

func TestOneUserCannotDeleteAnothersRule(t *testing.T) {
	s := testStore(t)
	owner := mustUser(t, s, 1, "owner")
	other := mustUser(t, s, 2, "other")
	r := mustRule(t, s, owner.ID, rules.KindOperator, "vr", 15)

	if err := s.DeleteRule(context.Background(), other.ID, r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user delete returned %v, want ErrNotFound", err)
	}
	if list, _ := s.ListRules(context.Background(), owner.ID); len(list) != 1 {
		t.Fatal("the rule was deleted by the wrong user")
	}
}

// ── Alerts: the idempotency invariant ─────────────────────────────────────────

func alertFor(r rules.Rule, delay int) rules.Alert {
	return rules.Alert{
		RuleID: r.ID, UserID: r.UserID, TrainNumber: 3001,
		DepartureDate: "2026-08-16", Operator: "vr",
		DelayMinutes: delay, Station: "Tampere",
	}
}

func TestAnAlertIsCreatedOnce(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	u := mustUser(t, s, 1, "cale")
	r := mustRule(t, s, u.ID, rules.KindOperator, "vr", 15)

	created, err := s.InsertAlerts(ctx, []rules.Alert{alertFor(r, 20)})
	if err != nil || len(created) != 1 {
		t.Fatalf("first insert created %d (err %v), want 1", len(created), err)
	}
}

func TestTheSecondPollDoesNotReAlert(t *testing.T) {
	// The core promise. A train 20 minutes late is still late on the next poll;
	// without the UNIQUE constraint every run would notify again.
	ctx := context.Background()
	s := testStore(t)
	u := mustUser(t, s, 1, "cale")
	r := mustRule(t, s, u.ID, rules.KindOperator, "vr", 15)

	_, _ = s.InsertAlerts(ctx, []rules.Alert{alertFor(r, 20)})
	created, err := s.InsertAlerts(ctx, []rules.Alert{alertFor(r, 20)})
	if err != nil {
		t.Fatalf("second insert errored: %v", err)
	}
	if len(created) != 0 {
		t.Fatalf("second poll created %d alerts, want 0", len(created))
	}
}

func TestAWorseningDelayStillDoesNotReAlert(t *testing.T) {
	// The dedupe key excludes the delay on purpose: otherwise every minute the
	// train slipped would be a fresh notification.
	ctx := context.Background()
	s := testStore(t)
	u := mustUser(t, s, 1, "cale")
	r := mustRule(t, s, u.ID, rules.KindOperator, "vr", 15)

	_, _ = s.InsertAlerts(ctx, []rules.Alert{alertFor(r, 20)})
	created, _ := s.InsertAlerts(ctx, []rules.Alert{alertFor(r, 95)})
	if len(created) != 0 {
		t.Fatalf("a worsening delay re-alerted %d times, want 0", len(created))
	}
}

func TestTheSameTrainOnTheNextDayAlertsAgain(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	u := mustUser(t, s, 1, "cale")
	r := mustRule(t, s, u.ID, rules.KindOperator, "vr", 15)

	_, _ = s.InsertAlerts(ctx, []rules.Alert{alertFor(r, 20)})
	tomorrow := alertFor(r, 20)
	tomorrow.DepartureDate = "2026-08-17"
	created, err := s.InsertAlerts(ctx, []rules.Alert{tomorrow})
	if err != nil || len(created) != 1 {
		t.Fatalf("next day created %d (err %v), want 1", len(created), err)
	}
}

func TestTwoRulesOnTheSameTrainBothAlert(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	u := mustUser(t, s, 1, "cale")
	byOperator := mustRule(t, s, u.ID, rules.KindOperator, "vr", 15)
	byNumber := mustRule(t, s, u.ID, rules.KindTrain, "3001", 10)

	created, err := s.InsertAlerts(ctx, []rules.Alert{alertFor(byOperator, 20), alertFor(byNumber, 20)})
	if err != nil || len(created) != 2 {
		t.Fatalf("got %d alerts (err %v), want one per rule", len(created), err)
	}
}

func TestAnUnparseableDepartureDateIsReported(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	u := mustUser(t, s, 1, "cale")
	r := mustRule(t, s, u.ID, rules.KindOperator, "vr", 15)
	bad := alertFor(r, 20)
	bad.DepartureDate = "16/08/2026"

	if _, err := s.InsertAlerts(ctx, []rules.Alert{bad}); err == nil {
		t.Fatal("expected an unparseable date to be reported rather than stored")
	}
}

func TestAlertsListNewestFirstAndScopedToTheUser(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	mine := mustUser(t, s, 1, "mine")
	theirs := mustUser(t, s, 2, "theirs")
	myRule := mustRule(t, s, mine.ID, rules.KindOperator, "vr", 15)
	theirRule := mustRule(t, s, theirs.ID, rules.KindOperator, "vr", 15)

	first := alertFor(myRule, 20)
	second := alertFor(myRule, 20)
	second.TrainNumber = 3002
	_, _ = s.InsertAlerts(ctx, []rules.Alert{first})
	_, _ = s.InsertAlerts(ctx, []rules.Alert{second})
	_, _ = s.InsertAlerts(ctx, []rules.Alert{alertFor(theirRule, 20)})

	got, err := s.ListAlerts(ctx, mine.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d alerts, want only this user's 2", len(got))
	}
	if got[0].ID < got[1].ID {
		t.Fatal("alerts should come back newest first")
	}
}

// ── Poll runs ─────────────────────────────────────────────────────────────────

func TestAPollRunRecordsItsOutcome(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	id, err := s.StartPollRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishPollRun(ctx, id, 22, 3, 1, nil); err != nil {
		t.Fatal(err)
	}
	var trains, alerts int
	var errText *string
	if err := s.Pool().QueryRow(ctx,
		`SELECT trains_seen, alerts_created, error FROM poll_runs WHERE id = $1`, id).
		Scan(&trains, &alerts, &errText); err != nil {
		t.Fatal(err)
	}
	if trains != 22 || alerts != 1 || errText != nil {
		t.Fatalf("got (%d, %d, %v), want (22, 1, nil)", trains, alerts, errText)
	}
}

func TestAFailedPollRunStoresItsError(t *testing.T) {
	// A silent failure should be visible as a stored error, not just as alerts
	// that quietly stopped arriving.
	ctx := context.Background()
	s := testStore(t)
	id, _ := s.StartPollRun(ctx)
	if err := s.FinishPollRun(ctx, id, 0, 0, 0, errors.New("upstream 503")); err != nil {
		t.Fatal(err)
	}
	var errText *string
	_ = s.Pool().QueryRow(ctx, `SELECT error FROM poll_runs WHERE id = $1`, id).Scan(&errText)
	if errText == nil || *errText != "upstream 503" {
		t.Fatalf("stored error = %v, want the upstream message", errText)
	}
}
