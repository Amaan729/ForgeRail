package pgstore_test

import (
	"context"
	"os"
	"testing"

	"github.com/Amaan729/ForgeRail/internal/ledger"
	"github.com/Amaan729/ForgeRail/internal/ledger/ledgertest"
	"github.com/Amaan729/ForgeRail/internal/pgstore"
)

// These tests need a throwaway database, e.g.
//
//	FORGERAIL_TEST_DATABASE_URL=postgres://localhost:55432/forgerail_test?sslmode=disable
//
// They truncate every table, so never point this at anything you care about.
func TestPostgresStore(t *testing.T) {
	url := os.Getenv("FORGERAIL_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("FORGERAIL_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgstore.Open(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pgstore.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// running it twice should be harmless
	if err := pgstore.Migrate(ctx, pool); err != nil {
		t.Fatalf("second migrate: %v", err)
	}

	ledgertest.Run(t, func(t *testing.T) ledger.Store {
		if _, err := pool.Exec(ctx, `TRUNCATE entries, transfers, accounts`); err != nil {
			t.Fatalf("truncate: %v", err)
		}
		if err := pgstore.EnsureSystemAccounts(ctx, pool); err != nil {
			t.Fatalf("seed: %v", err)
		}
		return pgstore.New(pool)
	})
}
