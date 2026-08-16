package poll

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/calemccammon/freight-alerts/internal/rules"
	"github.com/calemccammon/freight-alerts/internal/webhook"
)

type fakeFetcher struct {
	trains []rules.Train
	err    error
}

func (f *fakeFetcher) RunningCargoTrains(context.Context) ([]rules.Train, error) {
	return f.trains, f.err
}

type fakeStore struct {
	active []rules.Rule
	// suppress mimics the UNIQUE constraint: a key already present is not
	// returned as newly created.
	suppress map[string]bool
	inserted []rules.Alert
	statuses map[string]string
	runs     []struct {
		trains, rules, alerts int
		err                   error
	}
	purged     int64
	failActive error
	failInsert error
}

func newFakeStore(active ...rules.Rule) *fakeStore {
	return &fakeStore{active: active, suppress: map[string]bool{}, statuses: map[string]string{}}
}

func (f *fakeStore) ActiveRules(context.Context) ([]rules.Rule, error) {
	return f.active, f.failActive
}

func (f *fakeStore) InsertAlerts(_ context.Context, candidates []rules.Alert) ([]rules.Alert, error) {
	if f.failInsert != nil {
		return nil, f.failInsert
	}
	var created []rules.Alert
	for _, a := range candidates {
		if f.suppress[a.DedupeKey()] {
			continue
		}
		f.suppress[a.DedupeKey()] = true
		f.inserted = append(f.inserted, a)
		created = append(created, a)
	}
	return created, nil
}

func (f *fakeStore) MarkWebhookStatus(_ context.Context, ruleID int64, train int, date, status string) error {
	f.statuses[rules.Alert{RuleID: ruleID, TrainNumber: train, DepartureDate: date}.DedupeKey()] = status
	return nil
}

func (f *fakeStore) StartPollRun(context.Context) (int64, error) { return 1, nil }

func (f *fakeStore) FinishPollRun(_ context.Context, _ int64, trains, rulesEvaluated, alerts int, err error) error {
	f.runs = append(f.runs, struct {
		trains, rules, alerts int
		err                   error
	}{trains, rulesEvaluated, alerts, err})
	return nil
}

func (f *fakeStore) PurgeExpiredSessions(context.Context) (int64, error) { return f.purged, nil }

type fakeNotifier struct {
	sent []string
	err  error
}

func (f *fakeNotifier) Send(_ context.Context, url string, _ webhook.Payload) (string, error) {
	f.sent = append(f.sent, url)
	if f.err != nil {
		return "unreachable", f.err
	}
	return "ok_200", nil
}

func lateTrain(number int, operator string, delay int) rules.Train {
	actual := time.Date(2026, 8, 16, 9, 0, 0, 0, time.UTC)
	d := delay
	return rules.Train{
		TrainNumber: number, DepartureDate: "2026-08-16", Operator: operator,
		Rows: []rules.TimetableRow{{
			Station: "Tampere", Type: "ARRIVAL",
			ScheduledTime: actual, ActualTime: &actual, DifferenceInMinutes: &d,
		}},
	}
}

func testPoller(f *fakeFetcher, s *fakeStore, n *fakeNotifier) *Poller {
	return New(f, s, n, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func operatorRule(id int64, threshold int, hook string) rules.Rule {
	return rules.Rule{ID: id, UserID: 1, Kind: rules.KindOperator, Target: "vr",
		ThresholdMinutes: threshold, WebhookURL: hook, Active: true}
}

// ── Happy path ────────────────────────────────────────────────────────────────

func TestALateTrainCreatesAnAlert(t *testing.T) {
	fs := newFakeStore(operatorRule(7, 15, ""))
	p := testPoller(&fakeFetcher{trains: []rules.Train{lateTrain(3001, "vr", 22)}}, fs, &fakeNotifier{})

	got, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got.AlertsCreated != 1 || got.TrainsSeen != 1 || got.RulesEvaluated != 1 {
		t.Fatalf("result = %+v", got)
	}
}

func TestASecondRunDoesNotReAlert(t *testing.T) {
	// The end-to-end version of the invariant: the store suppresses, so the
	// second cycle creates nothing even though the train is still late.
	fs := newFakeStore(operatorRule(7, 15, ""))
	p := testPoller(&fakeFetcher{trains: []rules.Train{lateTrain(3001, "vr", 22)}}, fs, &fakeNotifier{})

	_, _ = p.Run(context.Background())
	second, err := p.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.AlertsCreated != 0 {
		t.Fatalf("second run created %d alerts, want 0", second.AlertsCreated)
	}
}

func TestAnOnTimeFleetProducesNothing(t *testing.T) {
	fs := newFakeStore(operatorRule(7, 15, ""))
	p := testPoller(&fakeFetcher{trains: []rules.Train{lateTrain(3001, "vr", 2)}}, fs, &fakeNotifier{})

	got, _ := p.Run(context.Background())
	if got.AlertsCreated != 0 {
		t.Fatalf("created %d alerts for an on-time fleet", got.AlertsCreated)
	}
}

func TestUpstreamIsNotCalledWhenNobodyIsWatching(t *testing.T) {
	// No rules means no reason to hit Digitraffic at all.
	fetcher := &fakeFetcher{err: errors.New("should not have been called")}
	fs := newFakeStore() // no active rules
	got, err := testPoller(fetcher, fs, &fakeNotifier{}).Run(context.Background())
	if err != nil {
		t.Fatalf("run errored: %v", err)
	}
	if got.TrainsSeen != 0 {
		t.Fatal("upstream was called with no active rules")
	}
}

// ── Webhooks ──────────────────────────────────────────────────────────────────

func TestOnlyNewAlertsTriggerAWebhook(t *testing.T) {
	fs := newFakeStore(operatorRule(7, 15, "https://hooks.example.com/x"))
	notifier := &fakeNotifier{}
	p := testPoller(&fakeFetcher{trains: []rules.Train{lateTrain(3001, "vr", 22)}}, fs, notifier)

	_, _ = p.Run(context.Background())
	_, _ = p.Run(context.Background()) // same train, still late

	if len(notifier.sent) != 1 {
		t.Fatalf("sent %d webhooks, want exactly 1 for the one new alert", len(notifier.sent))
	}
}

func TestARuleWithoutAWebhookSendsNothing(t *testing.T) {
	fs := newFakeStore(operatorRule(7, 15, ""))
	notifier := &fakeNotifier{}
	_, _ = testPoller(&fakeFetcher{trains: []rules.Train{lateTrain(3001, "vr", 22)}}, fs, notifier).
		Run(context.Background())

	if len(notifier.sent) != 0 {
		t.Fatalf("sent %d webhooks for a rule with no URL", len(notifier.sent))
	}
}

func TestAFailedWebhookDoesNotFailTheRun(t *testing.T) {
	// The alert is already persisted and visible in the feed; one bad
	// subscriber URL must not stop everyone else's notifications.
	fs := newFakeStore(operatorRule(7, 15, "https://broken.example.com/x"))
	notifier := &fakeNotifier{err: errors.New("connection refused")}
	got, err := testPoller(&fakeFetcher{trains: []rules.Train{lateTrain(3001, "vr", 22)}}, fs, notifier).
		Run(context.Background())

	if err != nil {
		t.Fatalf("a webhook failure failed the whole run: %v", err)
	}
	if got.AlertsCreated != 1 {
		t.Fatal("the alert should still have been created")
	}
	if got.WebhooksSent != 0 {
		t.Fatal("a failed delivery should not count as sent")
	}
}

func TestWebhookOutcomeIsRecordedEitherWay(t *testing.T) {
	fs := newFakeStore(operatorRule(7, 15, "https://broken.example.com/x"))
	notifier := &fakeNotifier{err: errors.New("nope")}
	_, _ = testPoller(&fakeFetcher{trains: []rules.Train{lateTrain(3001, "vr", 22)}}, fs, notifier).
		Run(context.Background())

	key := rules.Alert{RuleID: 7, TrainNumber: 3001, DepartureDate: "2026-08-16"}.DedupeKey()
	if fs.statuses[key] != "unreachable" {
		t.Fatalf("recorded status = %q, want the failure recorded", fs.statuses[key])
	}
}

// ── Failure recording ─────────────────────────────────────────────────────────

func TestAnUpstreamFailureIsRecordedOnTheRun(t *testing.T) {
	// A service that has quietly stopped alerting must look different from one
	// with nothing to report.
	fs := newFakeStore(operatorRule(7, 15, ""))
	p := testPoller(&fakeFetcher{err: errors.New("upstream 503")}, fs, &fakeNotifier{})

	if _, err := p.Run(context.Background()); err == nil {
		t.Fatal("expected the upstream failure to surface")
	}
	if len(fs.runs) != 1 || fs.runs[0].err == nil {
		t.Fatalf("poll run recorded %+v, want the error stored", fs.runs)
	}
}

func TestASuccessfulRunRecordsItsCounts(t *testing.T) {
	fs := newFakeStore(operatorRule(7, 15, ""))
	_, _ = testPoller(&fakeFetcher{trains: []rules.Train{
		lateTrain(3001, "vr", 22), lateTrain(3002, "vr", 1),
	}}, fs, &fakeNotifier{}).Run(context.Background())

	if len(fs.runs) != 1 {
		t.Fatalf("recorded %d runs, want 1", len(fs.runs))
	}
	run := fs.runs[0]
	if run.trains != 2 || run.rules != 1 || run.alerts != 1 || run.err != nil {
		t.Fatalf("run = %+v, want 2 trains / 1 rule / 1 alert / no error", run)
	}
}

func TestAStoreFailureIsSurfacedAndRecorded(t *testing.T) {
	fs := newFakeStore(operatorRule(7, 15, ""))
	fs.failInsert = errors.New("deadlock")
	p := testPoller(&fakeFetcher{trains: []rules.Train{lateTrain(3001, "vr", 22)}}, fs, &fakeNotifier{})

	if _, err := p.Run(context.Background()); err == nil {
		t.Fatal("expected the insert failure to surface")
	}
	if fs.runs[0].err == nil {
		t.Fatal("the failure should be recorded against the poll run")
	}
}

func TestAFailureLoadingRulesStopsTheRun(t *testing.T) {
	fs := newFakeStore()
	fs.failActive = errors.New("db down")
	fetcher := &fakeFetcher{err: errors.New("should not have been called")}

	if _, err := testPoller(fetcher, fs, &fakeNotifier{}).Run(context.Background()); err == nil {
		t.Fatal("expected the rule-load failure to surface")
	}
}
