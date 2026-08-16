// Package rules holds the alerting decision: given a train and a watch rule,
// should this fire, and what identifies the alert.
//
// Everything here is pure. No database, no HTTP, no clock reads that are not
// passed in -- which is what makes the interesting cases (a train that is late
// at one station and recovered at the next, a rule that already fired, a
// threshold exactly on the boundary) testable without infrastructure.
package rules

import (
	"fmt"
	"strings"
	"time"
)

// Kind is what a rule watches. Deliberately a small closed set: an operator or
// a station is a coarse subscription, a train number is a precise one, and
// anything more expressive belongs in a query language rather than a rule row.
type Kind string

const (
	KindTrain    Kind = "train"    // one train number
	KindOperator Kind = "operator" // every train run by an operator, e.g. "vr"
	KindStation  Kind = "station"  // every train whose timetable touches a station
)

var validKinds = map[Kind]bool{KindTrain: true, KindOperator: true, KindStation: true}

func (k Kind) Valid() bool { return validKinds[k] }

// Rule is one user's subscription.
type Rule struct {
	ID               int64
	UserID           int64
	Kind             Kind
	Target           string // train number, operator short code, or station name
	ThresholdMinutes int
	WebhookURL       string
	Active           bool
}

// Validate rejects a rule before it reaches the database. The schema enforces
// shape; this enforces meaning.
func (r Rule) Validate() error {
	if !r.Kind.Valid() {
		return fmt.Errorf("unknown rule kind %q", r.Kind)
	}
	if strings.TrimSpace(r.Target) == "" {
		return fmt.Errorf("rule target must not be empty")
	}
	if r.ThresholdMinutes < 1 {
		// A threshold of zero would fire on every train on every poll, which is
		// a subscription to noise rather than an alert.
		return fmt.Errorf("threshold must be at least 1 minute, got %d", r.ThresholdMinutes)
	}
	if r.ThresholdMinutes > 24*60 {
		return fmt.Errorf("threshold must be under 24 hours, got %d minutes", r.ThresholdMinutes)
	}
	return nil
}

// TimetableRow is one scheduled stop. DifferenceInMinutes is positive when late.
type TimetableRow struct {
	Station             string
	Type                string // "ARRIVAL" or "DEPARTURE"
	ScheduledTime       time.Time
	ActualTime          *time.Time
	DifferenceInMinutes *int
}

// Realised reports whether this stop has actually happened. Upstream also
// publishes estimates for future stops; alerting on those would mean alerting
// on a prediction that the next poll may retract.
func (r TimetableRow) Realised() bool {
	return r.ActualTime != nil && r.DifferenceInMinutes != nil
}

// Train is a running cargo service.
type Train struct {
	TrainNumber   int
	DepartureDate string // ISO date, the second half of the upstream composite key
	Operator      string
	Rows          []TimetableRow
}

// CurrentDelay is the delay at the most recent stop the train has actually
// reached, and whether any such stop exists.
//
// Not the worst delay across the journey: a train that lost 30 minutes early
// and made it back up is not currently late, and alerting on its worst moment
// would keep firing about a problem that resolved.
func (t Train) CurrentDelay() (minutes int, station string, ok bool) {
	var latest *TimetableRow
	for i := range t.Rows {
		row := &t.Rows[i]
		if !row.Realised() {
			continue
		}
		if latest == nil || row.ActualTime.After(*latest.ActualTime) {
			latest = row
		}
	}
	if latest == nil {
		return 0, "", false
	}
	return *latest.DifferenceInMinutes, latest.Station, true
}

// TouchesStation reports whether the train's timetable includes a station,
// compared case-insensitively because upstream station names are Finnish and
// arrive with inconsistent casing.
func (t Train) TouchesStation(name string) bool {
	want := strings.ToLower(strings.TrimSpace(name))
	for _, row := range t.Rows {
		if strings.ToLower(row.Station) == want {
			return true
		}
	}
	return false
}

// Matches reports whether the rule is about this train at all, ignoring delay.
func (r Rule) Matches(t Train) bool {
	switch r.Kind {
	case KindTrain:
		return strings.TrimSpace(r.Target) == fmt.Sprint(t.TrainNumber)
	case KindOperator:
		return strings.EqualFold(strings.TrimSpace(r.Target), t.Operator)
	case KindStation:
		return t.TouchesStation(r.Target)
	default:
		return false
	}
}

// Alert is a rule firing for one train on one departure date.
type Alert struct {
	RuleID        int64
	UserID        int64
	TrainNumber   int
	DepartureDate string
	Operator      string
	DelayMinutes  int
	Station       string
	WebhookURL    string
}

// DedupeKey identifies an alert for the purpose of not sending it twice.
//
// Deliberately excludes the delay: a train that is 20 minutes late stays 20
// minutes late across polls, and keying on the delay would fire again on every
// minute it slips. One alert per rule per train per departure date is the
// promise. The same tuple is a UNIQUE constraint in the schema, so the database
// -- not this function -- is what actually enforces it when two runs overlap.
func (a Alert) DedupeKey() string {
	return fmt.Sprintf("%d:%d:%s", a.RuleID, a.TrainNumber, a.DepartureDate)
}

// Evaluate returns the alerts a set of rules produces for a set of trains.
//
// It does not know what has already fired. Suppression is the store's job,
// because only the database can decide it correctly when runs overlap.
func Evaluate(rs []Rule, trains []Train) []Alert {
	var alerts []Alert
	for _, rule := range rs {
		if !rule.Active {
			continue
		}
		for _, train := range trains {
			if !rule.Matches(train) {
				continue
			}
			delay, station, ok := train.CurrentDelay()
			if !ok || delay < rule.ThresholdMinutes {
				continue
			}
			alerts = append(alerts, Alert{
				RuleID:        rule.ID,
				UserID:        rule.UserID,
				TrainNumber:   train.TrainNumber,
				DepartureDate: train.DepartureDate,
				Operator:      train.Operator,
				DelayMinutes:  delay,
				Station:       station,
				WebhookURL:    rule.WebhookURL,
			})
		}
	}
	return alerts
}
