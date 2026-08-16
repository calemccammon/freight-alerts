// Command freightalerts is the whole service: one binary, three subcommands.
//
//	freightalerts migrate   apply schema migrations
//	freightalerts poll      run one poll cycle and exit (what cron invokes)
//	freightalerts serve     run the HTTP API
//
// Poll is a single pass rather than a daemon with its own timer, because the
// schedule lives outside the process. That is what makes the whole service
// runnable on free scheduled compute with nothing left running between cycles.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/calemccammon/freight-alerts/internal/api"
	"github.com/calemccammon/freight-alerts/internal/auth"
	"github.com/calemccammon/freight-alerts/internal/digitraffic"
	"github.com/calemccammon/freight-alerts/internal/poll"
	"github.com/calemccammon/freight-alerts/internal/store"
	"github.com/calemccammon/freight-alerts/internal/webhook"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "migrate":
		err = runMigrate(ctx, log)
	case "poll":
		err = runPoll(ctx, log)
	case "serve":
		err = runServe(ctx, log)
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}

	if err != nil {
		log.Error("command failed", "command", os.Args[1], "err", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `freightalerts — delay alerts for Finnish cargo rail

  migrate   apply schema migrations, then exit
  poll      run one poll cycle, then exit (invoke from cron)
  serve     run the HTTP API

Environment:
  DATABASE_URL           required — Postgres connection string
  PORT                   serve: listen port (default 8080)
  GITHUB_CLIENT_ID       serve: OAuth app client id
  GITHUB_CLIENT_SECRET   serve: OAuth app client secret
  GITHUB_REDIRECT_URL    serve: callback URL registered with the OAuth app
  ALLOWED_LOGINS         serve: comma-separated GitHub logins permitted to
                         sign in; unset means any GitHub account may
  INSECURE_COOKIES       serve: set to 1 for local http development
`)
}

func mustDSN() (string, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return "", errors.New("DATABASE_URL is not set")
	}
	return dsn, nil
}

func openStore(ctx context.Context) (*store.Store, error) {
	dsn, err := mustDSN()
	if err != nil {
		return nil, err
	}
	return store.Open(ctx, dsn)
}

func runMigrate(ctx context.Context, log *slog.Logger) error {
	s, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close()

	applied, err := store.Migrate(ctx, s.Pool())
	if err != nil {
		return err
	}
	if len(applied) == 0 {
		log.Info("schema already up to date")
	} else {
		log.Info("migrations applied", "versions", applied)
	}
	return nil
}

func runPoll(ctx context.Context, log *slog.Logger) error {
	s, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close()

	// Migrating on every poll keeps the scheduled path self-sufficient: there is
	// no separate deploy step that could be skipped, and it is a no-op once the
	// schema is current.
	if _, err := store.Migrate(ctx, s.Pool()); err != nil {
		return fmt.Errorf("migrate before poll: %w", err)
	}

	// Bounded so a hung upstream cannot leave a scheduled run consuming its
	// whole time budget.
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	poller := poll.New(
		digitraffic.New("", 30*time.Second),
		s,
		webhook.New(),
		log,
	)
	result, err := poller.Run(ctx)
	if err != nil {
		return err
	}
	log.Info("poll finished",
		"trains", result.TrainsSeen, "rules", result.RulesEvaluated,
		"alerts", result.AlertsCreated, "webhooks", result.WebhooksSent)
	return nil
}

func runServe(ctx context.Context, log *slog.Logger) error {
	s, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close()

	if _, err := store.Migrate(ctx, s.Pool()); err != nil {
		return fmt.Errorf("migrate before serve: %w", err)
	}

	github := auth.NewGitHub(
		os.Getenv("GITHUB_CLIENT_ID"),
		os.Getenv("GITHUB_CLIENT_SECRET"),
		os.Getenv("GITHUB_REDIRECT_URL"),
	)
	if !github.Configured() {
		// Not fatal: the poller does not need OAuth, and a deployment without
		// it should still serve /healthz and return a clear 503 on sign-in
		// rather than a broken redirect.
		log.Warn("GitHub OAuth is not configured; sign-in will return 503")
	}

	// Logged at startup because an unset or mistyped ALLOWED_LOGINS opens
	// sign-in to any GitHub account, and that must be visible in the logs
	// rather than discovered later from an unexpected user row.
	allow := api.ParseAllowlist(os.Getenv("ALLOWED_LOGINS"))
	if allow.Open() {
		log.Warn("ALLOWED_LOGINS is not set; sign-in is open to any GitHub account")
	} else {
		log.Info("sign-in restricted", "permitted_logins", allow.Size())
	}

	secure := os.Getenv("INSECURE_COOKIES") != "1"
	handler := api.New(s, github, log, secure, allow).Routes()

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	if _, err := strconv.Atoi(port); err != nil {
		return fmt.Errorf("PORT must be numeric, got %q", port)
	}

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "port", port, "secure_cookies", secure)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
