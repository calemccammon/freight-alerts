// Package poll runs one cycle: read upstream, evaluate rules, persist what is
// new, notify.
//
// It is written as a single pass rather than a loop with its own timer, because
// the schedule lives outside the process -- a cron trigger invokes the binary.
// That means a run can overlap with the previous one if upstream is slow, which
// is precisely why suppression is a database constraint and not a variable in
// this package.
package poll

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/calemccammon/freight-alerts/internal/rules"
	"github.com/calemccammon/freight-alerts/internal/webhook"
)

type Fetcher interface {
	RunningCargoTrains(ctx context.Context) ([]rules.Train, error)
}

type Store interface {
	ActiveRules(ctx context.Context) ([]rules.Rule, error)
	InsertAlerts(ctx context.Context, candidates []rules.Alert) ([]rules.Alert, error)
	MarkWebhookStatus(ctx context.Context, ruleID int64, trainNumber int, departureDate, status string) error
	StartPollRun(ctx context.Context) (int64, error)
	FinishPollRun(ctx context.Context, id int64, trains, rulesEvaluated, alertsCreated int, runErr error) error
	PurgeExpiredSessions(ctx context.Context) (int64, error)
}

type Notifier interface {
	Send(ctx context.Context, url string, p webhook.Payload) (string, error)
}

type Poller struct {
	fetch  Fetcher
	store  Store
	notify Notifier
	log    *slog.Logger
}

func New(f Fetcher, s Store, n Notifier, log *slog.Logger) *Poller {
	return &Poller{fetch: f, store: s, notify: n, log: log}
}

// Result summarises one cycle.
type Result struct {
	TrainsSeen     int
	RulesEvaluated int
	AlertsCreated  int
	WebhooksSent   int
}

// Run executes one cycle. Every outcome, including failure, is recorded against
// a poll_runs row so a service that has quietly stopped alerting looks different
// from one with nothing to report.
func (p *Poller) Run(ctx context.Context) (Result, error) {
	var result Result

	runID, err := p.store.StartPollRun(ctx)
	if err != nil {
		return result, fmt.Errorf("start poll run: %w", err)
	}
	// Named return so the deferred finish records whatever actually happened.
	defer func() {
		if err := p.store.FinishPollRun(ctx, runID,
			result.TrainsSeen, result.RulesEvaluated, result.AlertsCreated, err); err != nil {
			p.log.Error("record poll run", "err", err)
		}
	}()

	activeRules, err := p.store.ActiveRules(ctx)
	if err != nil {
		return result, fmt.Errorf("load rules: %w", err)
	}
	result.RulesEvaluated = len(activeRules)
	if len(activeRules) == 0 {
		// Nobody is watching anything; skip the upstream call entirely.
		p.log.Info("no active rules; skipping upstream fetch")
		return result, nil
	}

	trains, err := p.fetch.RunningCargoTrains(ctx)
	if err != nil {
		return result, fmt.Errorf("fetch trains: %w", err)
	}
	result.TrainsSeen = len(trains)

	candidates := rules.Evaluate(activeRules, trains)
	if len(candidates) == 0 {
		p.log.Info("poll complete", "trains", len(trains), "rules", len(activeRules), "alerts", 0)
		return result, nil
	}

	// The database decides which of these are actually new. Anything it does
	// not return has already been alerted for this rule, train and day.
	created, err := p.store.InsertAlerts(ctx, candidates)
	if err != nil {
		return result, fmt.Errorf("persist alerts: %w", err)
	}
	result.AlertsCreated = len(created)

	// Only genuinely new alerts get a webhook, which is the whole reason
	// InsertAlerts returns rows rather than a count.
	for _, alert := range created {
		if alert.WebhookURL == "" {
			continue
		}
		status, sendErr := p.notify.Send(ctx, alert.WebhookURL, webhook.Payload{
			TrainNumber:   alert.TrainNumber,
			DepartureDate: alert.DepartureDate,
			Operator:      alert.Operator,
			DelayMinutes:  alert.DelayMinutes,
			Station:       alert.Station,
			RuleID:        alert.RuleID,
		})
		if sendErr != nil {
			// A failed webhook must not fail the run: the alert is already
			// persisted and visible in the feed, and one bad subscriber URL
			// should not stop everyone else's notifications.
			p.log.Warn("webhook delivery failed",
				"rule", alert.RuleID, "train", alert.TrainNumber, "status", status, "err", sendErr)
		} else {
			result.WebhooksSent++
		}
		if err := p.store.MarkWebhookStatus(ctx,
			alert.RuleID, alert.TrainNumber, alert.DepartureDate, status); err != nil {
			p.log.Warn("record webhook status", "err", err)
		}
	}

	// Housekeeping rides along with the poll so there is no second scheduled
	// job to forget about.
	if purged, err := p.store.PurgeExpiredSessions(ctx); err != nil {
		p.log.Warn("purge sessions", "err", err)
	} else if purged > 0 {
		p.log.Info("purged expired sessions", "count", purged)
	}

	p.log.Info("poll complete",
		"trains", result.TrainsSeen, "rules", result.RulesEvaluated,
		"alerts", result.AlertsCreated, "webhooks", result.WebhooksSent)
	return result, nil
}
