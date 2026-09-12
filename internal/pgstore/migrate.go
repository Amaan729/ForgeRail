package pgstore

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Amaan729/ForgeRail/internal/ledger"
	"github.com/Amaan729/ForgeRail/migrations"
)

// migrationLockID is an arbitrary key for pg_advisory_xact_lock so two
// instances starting at once don't both try to apply the same file.
const migrationLockID = 7_331_001

// Migrate applies any embedded migrations that have not run yet, then makes
// sure the system accounts exist.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	files, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)

	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
			version    text PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
			return err
		}
		for _, f := range files {
			version := strings.TrimSuffix(f, ".sql")
			var done bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&done); err != nil {
				return err
			}
			if done {
				continue
			}
			body, err := migrations.FS.ReadFile(f)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, string(body)); err != nil {
				return fmt.Errorf("migration %s: %w", f, err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return EnsureSystemAccounts(ctx, pool)
}

// EnsureSystemAccounts inserts the settlement/on-chain accounts if missing.
func EnsureSystemAccounts(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO accounts (id, name, allow_negative) VALUES
			($1, 'on-chain deposits', true),
			($2, 'withdrawals in flight', false),
			($3, 'on-chain withdrawals', false)
		ON CONFLICT (id) DO NOTHING`,
		ledger.AccountOnchainIn, ledger.AccountSettlement, ledger.AccountOnchainOut)
	return err
}
