package store

import (
	"context"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/calemccammon/freight-alerts/migrations"
)

// Migrate applies any embedded migration the database has not seen.
//
// Hand-rolled rather than goose or golang-migrate, for one concrete reason:
// both pull driver packages for every dialect they support, so a Postgres-only
// service ends up shipping a SQLite driver and its transitive tree. The whole
// mechanism is forty lines and one table, and it does the two things that
// actually matter -- each migration runs inside a transaction, and an advisory
// lock stops two instances racing on the same database at deploy time.
//
// Deliberately forward-only. There is no down path: rolling a schema backwards
// in a service that has already written rows is rarely the right recovery, and
// a Down section nobody exercises is worse than none.
func Migrate(ctx context.Context, pool *pgxpool.Pool) (applied []string, err error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()

	// Serialize concurrent deploys. Arbitrary constant, scoped to this database.
	const lockID = 8_242_119
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockID); err != nil {
		return nil, fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		if _, unlockErr := conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, lockID); unlockErr != nil && err == nil {
			err = fmt.Errorf("release migration lock: %w", unlockErr)
		}
	}()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}

	names, err := migrationNames()
	if err != nil {
		return nil, err
	}

	for _, name := range names {
		var seen bool
		if err := conn.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, name).
			Scan(&seen); err != nil {
			return nil, fmt.Errorf("check %s: %w", name, err)
		}
		if seen {
			continue
		}

		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}

		// One transaction per migration: a failure leaves the database on the
		// last complete version rather than half-way through this one.
		err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(body)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, name)
			return err
		})
		if err != nil {
			return applied, fmt.Errorf("apply %s: %w", name, err)
		}
		applied = append(applied, name)
	}
	return applied, nil
}

func migrationNames() ([]string, error) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && len(e.Name()) > 4 && e.Name()[len(e.Name())-4:] == ".sql" {
			names = append(names, e.Name())
		}
	}
	// Lexical order, which the zero-padded numeric prefixes make chronological.
	sort.Strings(names)
	return names, nil
}
