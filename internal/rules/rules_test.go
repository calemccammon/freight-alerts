package rules

import (
	"testing"
	"time"
)

func at(hour, minute int) time.Time {
	return time.Date(2026, 8, 16, hour, minute, 0, 0, time.UTC)
}

func realised(station string, actual time.Time, diff int) TimetableRow {
	return TimetableRow{
		Station: station, Type: "ARRIVAL",
		ScheduledTime: actual.Add(-time.Duration(diff) * time.Minute),
		ActualTime:    &actual, DifferenceInMinutes: &diff,
	}
}

func scheduled(station string, when time.Time) TimetableRow {
	return TimetableRow{Station: station, Type: "ARRIVAL", ScheduledTime: when}
}

func train(number int, operator string, rows ...TimetableRow) Train {
	return Train{TrainNumber: number, DepartureDate: "2026-08-16", Operator: operator, Rows: rows}
}

func rule(id int64, kind Kind, target string, threshold int) Rule {
	return Rule{ID: id, UserID: 1, Kind: kind, Target: target, ThresholdMinutes: threshold, Active: true}
}

// ── Validation ────────────────────────────────────────────────────────────────

func TestValidRuleIsAccepted(t *testing.T) {
	if err := rule(1, KindOperator, "vr", 15).Validate(); err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
}

func TestUnknownKindIsRejected(t *testing.T) {
	if err := rule(1, Kind("corridor"), "x", 15).Validate(); err == nil {
		t.Fatal("expected an unknown kind to be rejected")
	}
}

func TestEmptyTargetIsRejected(t *testing.T) {
	if err := rule(1, KindTrain, "   ", 15).Validate(); err == nil {
		t.Fatal("expected a blank target to be rejected")
	}
}

func TestZeroThresholdIsRejected(t *testing.T) {
	// Threshold 0 fires on every matching train on every poll, which is a
	// subscription to noise rather than an alert.
	if err := rule(1, KindOperator, "vr", 0).Validate(); err == nil {
		t.Fatal("expected a zero threshold to be rejected")
	}
}

func TestAbsurdThresholdIsRejected(t *testing.T) {
	if err := rule(1, KindOperator, "vr", 24*60+1).Validate(); err == nil {
		t.Fatal("expected a threshold over 24h to be rejected")
	}
}

// ── Current delay ─────────────────────────────────────────────────────────────

func TestCurrentDelayUsesTheMostRecentRealisedStop(t *testing.T) {
	tr := train(3001, "vr",
		realised("Tampere", at(9, 0), 30),
		realised("Riihimäki", at(10, 0), 5),
	)
	delay, station, ok := tr.CurrentDelay()
	if !ok || delay != 5 || station != "Riihimäki" {
		t.Fatalf("got (%d, %q, %v), want (5, Riihimäki, true)", delay, station, ok)
	}
}

func TestARecoveredTrainIsNotReportedAtItsWorst(t *testing.T) {
	// The whole reason CurrentDelay is not WorstDelay: a train that lost 40
	// minutes and made them back up is not currently late, and alerting on the
	// worst moment would keep firing about a resolved problem.
	tr := train(3001, "vr",
		realised("Oulu", at(6, 0), 40),
		realised("Seinäjoki", at(9, 0), 0),
	)
	delay, _, ok := tr.CurrentDelay()
	if !ok || delay != 0 {
		t.Fatalf("got delay %d, want 0", delay)
	}
}

func TestUnrealisedRowsAreIgnored(t *testing.T) {
	// Upstream publishes estimates for stops not yet reached. Alerting on those
	// means alerting on a prediction the next poll may retract.
	tr := train(3001, "vr",
		realised("Tampere", at(9, 0), 3),
		scheduled("Helsinki", at(11, 0)),
	)
	delay, station, ok := tr.CurrentDelay()
	if !ok || delay != 3 || station != "Tampere" {
		t.Fatalf("got (%d, %q, %v), want (3, Tampere, true)", delay, station, ok)
	}
}

func TestATrainThatHasNotReachedAnyStopHasNoDelay(t *testing.T) {
	tr := train(3001, "vr", scheduled("Tampere", at(9, 0)))
	if _, _, ok := tr.CurrentDelay(); ok {
		t.Fatal("expected no realised delay")
	}
}

func TestARowMissingItsDifferenceIsNotRealised(t *testing.T) {
	actual := at(9, 0)
	tr := train(3001, "vr", TimetableRow{Station: "Tampere", ActualTime: &actual})
	if _, _, ok := tr.CurrentDelay(); ok {
		t.Fatal("a row without differenceInMinutes must not count as realised")
	}
}

// ── Matching ──────────────────────────────────────────────────────────────────

func TestTrainRuleMatchesByNumber(t *testing.T) {
	tr := train(3001, "vr", realised("Tampere", at(9, 0), 20))
	if !rule(1, KindTrain, "3001", 5).Matches(tr) {
		t.Fatal("expected train 3001 to match")
	}
	if rule(1, KindTrain, "3002", 5).Matches(tr) {
		t.Fatal("expected train 3002 not to match")
	}
}

func TestOperatorRuleIsCaseInsensitive(t *testing.T) {
	tr := train(3001, "VR", realised("Tampere", at(9, 0), 20))
	if !rule(1, KindOperator, "vr", 5).Matches(tr) {
		t.Fatal("operator match should ignore case")
	}
}

func TestStationRuleMatchesAnyStopOnTheRoute(t *testing.T) {
	tr := train(3001, "vr",
		realised("Kouvola", at(8, 0), 2),
		realised("Riihimäki", at(9, 0), 20),
	)
	if !rule(1, KindStation, "kouvola", 5).Matches(tr) {
		t.Fatal("station match should ignore case and match any stop")
	}
	if rule(1, KindStation, "Oulu", 5).Matches(tr) {
		t.Fatal("a station not on the route must not match")
	}
}

// ── Evaluation ────────────────────────────────────────────────────────────────

func TestATrainOverThresholdProducesAnAlert(t *testing.T) {
	trains := []Train{train(3001, "vr", realised("Tampere", at(9, 0), 20))}
	alerts := Evaluate([]Rule{rule(7, KindOperator, "vr", 15)}, trains)
	if len(alerts) != 1 {
		t.Fatalf("got %d alerts, want 1", len(alerts))
	}
	got := alerts[0]
	if got.RuleID != 7 || got.TrainNumber != 3001 || got.DelayMinutes != 20 || got.Station != "Tampere" {
		t.Fatalf("unexpected alert: %+v", got)
	}
}

func TestThresholdIsInclusive(t *testing.T) {
	// A rule for "15 minutes late" should fire at exactly 15, not only past it.
	trains := []Train{train(3001, "vr", realised("Tampere", at(9, 0), 15))}
	if len(Evaluate([]Rule{rule(1, KindOperator, "vr", 15)}, trains)) != 1 {
		t.Fatal("expected a delay equal to the threshold to fire")
	}
}

func TestJustUnderThresholdDoesNotFire(t *testing.T) {
	trains := []Train{train(3001, "vr", realised("Tampere", at(9, 0), 14))}
	if got := Evaluate([]Rule{rule(1, KindOperator, "vr", 15)}, trains); len(got) != 0 {
		t.Fatalf("got %d alerts, want 0", len(got))
	}
}

func TestAnEarlyTrainNeverFires(t *testing.T) {
	trains := []Train{train(3001, "vr", realised("Tampere", at(9, 0), -6))}
	if got := Evaluate([]Rule{rule(1, KindOperator, "vr", 1)}, trains); len(got) != 0 {
		t.Fatalf("a train running early produced %d alerts", len(got))
	}
}

func TestInactiveRulesAreSkipped(t *testing.T) {
	r := rule(1, KindOperator, "vr", 5)
	r.Active = false
	trains := []Train{train(3001, "vr", realised("Tampere", at(9, 0), 60))}
	if got := Evaluate([]Rule{r}, trains); len(got) != 0 {
		t.Fatalf("an inactive rule produced %d alerts", len(got))
	}
}

func TestOneTrainCanFireSeveralRules(t *testing.T) {
	trains := []Train{train(3001, "vr", realised("Tampere", at(9, 0), 30))}
	alerts := Evaluate([]Rule{
		rule(1, KindOperator, "vr", 15),
		rule(2, KindTrain, "3001", 10),
		rule(3, KindStation, "Tampere", 5),
	}, trains)
	if len(alerts) != 3 {
		t.Fatalf("got %d alerts, want one per matching rule", len(alerts))
	}
}

func TestATrainWithNoRealisedStopsNeverFires(t *testing.T) {
	trains := []Train{train(3001, "vr", scheduled("Tampere", at(9, 0)))}
	if got := Evaluate([]Rule{rule(1, KindOperator, "vr", 1)}, trains); len(got) != 0 {
		t.Fatalf("got %d alerts from a train with no realised stop", len(got))
	}
}

// ── Dedupe key ────────────────────────────────────────────────────────────────

func TestDedupeKeyIgnoresTheDelay(t *testing.T) {
	// A late train stays late across polls. Keying on the delay would fire again
	// every time it slipped another minute.
	base := Alert{RuleID: 1, TrainNumber: 3001, DepartureDate: "2026-08-16", DelayMinutes: 20}
	worse := base
	worse.DelayMinutes = 45
	if base.DedupeKey() != worse.DedupeKey() {
		t.Fatal("the same rule/train/date must dedupe regardless of delay")
	}
}

func TestDedupeKeySeparatesRulesTrainsAndDates(t *testing.T) {
	base := Alert{RuleID: 1, TrainNumber: 3001, DepartureDate: "2026-08-16"}
	for name, other := range map[string]Alert{
		"different rule":  {RuleID: 2, TrainNumber: 3001, DepartureDate: "2026-08-16"},
		"different train": {RuleID: 1, TrainNumber: 3002, DepartureDate: "2026-08-16"},
		"different date":  {RuleID: 1, TrainNumber: 3001, DepartureDate: "2026-08-17"},
	} {
		if base.DedupeKey() == other.DedupeKey() {
			t.Fatalf("%s should not share a dedupe key", name)
		}
	}
}

// The same train number runs every day, so the departure date is load-bearing:
// without it, today's delay would suppress tomorrow's alert entirely.
func TestTheSameTrainOnTheNextDayIsANewAlert(t *testing.T) {
	today := Alert{RuleID: 1, TrainNumber: 3001, DepartureDate: "2026-08-16"}
	tomorrow := Alert{RuleID: 1, TrainNumber: 3001, DepartureDate: "2026-08-17"}
	if today.DedupeKey() == tomorrow.DedupeKey() {
		t.Fatal("consecutive days must alert separately")
	}
}
