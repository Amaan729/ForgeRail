// Command replay hammers the ledger with a deterministic batch of transfers
// sent the way real clients send them: duplicated, raced against each other,
// and cancelled mid-flight then retried. Afterwards it checks that nothing
// was debited twice and that balances match both the plan and the entries.
//
//	go run ./cmd/replay -database-url $DATABASE_URL -reset -n 100000
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Amaan729/ForgeRail/internal/ledger"
	"github.com/Amaan729/ForgeRail/internal/money"
	"github.com/Amaan729/ForgeRail/internal/pgstore"
)

type options struct {
	dbURL        string
	n            int
	accounts     int
	workers      int
	dupRate      float64
	raceRate     float64
	cancelRate   float64
	withdrawRate float64
	releaseRate  float64
	seed         uint64
	reset        bool
}

func main() {
	var o options
	flag.StringVar(&o.dbURL, "database-url", os.Getenv("DATABASE_URL"), "Postgres URL (empty = in-memory store, fewer checks)")
	flag.IntVar(&o.n, "n", 100_000, "number of transfers")
	flag.IntVar(&o.accounts, "accounts", 500, "number of user accounts")
	flag.IntVar(&o.workers, "workers", 64, "concurrent clients")
	flag.Float64Var(&o.dupRate, "dup-rate", 0.3, "chance a transfer is sent more than once")
	flag.Float64Var(&o.raceRate, "race-rate", 0.5, "chance duplicates are sent concurrently instead of one after another")
	flag.Float64Var(&o.cancelRate, "cancel-rate", 0.05, "chance the first attempt is cancelled mid-flight")
	flag.Float64Var(&o.withdrawRate, "withdraw-rate", 0.1, "fraction of transfers that are withdrawals")
	flag.Float64Var(&o.releaseRate, "release-rate", 0.2, "fraction of withdrawals that fail and get released")
	flag.Uint64Var(&o.seed, "seed", 1, "RNG seed")
	flag.BoolVar(&o.reset, "reset", false, "TRUNCATE all ledger tables first (Postgres only!)")
	flag.Parse()

	if err := run(context.Background(), o); err != nil {
		fmt.Fprintln(os.Stderr, "replay:", err)
		os.Exit(1)
	}
}

type op struct {
	req         ledger.TransferRequest
	sends       int  // total CreateTransfer calls
	race        bool // send duplicates concurrently
	cancelFirst bool // first attempt gets a tiny deadline
	release     bool // withdrawal outcome
	settleCalls int  // how many times Settle/Release get called (activity retries)
}

type stats struct {
	requests, duplicates, raced, cancelled, cancelledCommitted atomic.Int64
	lifecycleCalls, transientErrs, fresh                       atomic.Int64
}

func run(ctx context.Context, o options) error {
	var store ledger.Store
	var pool *pgxpool.Pool
	if o.dbURL != "" {
		cfg, err := pgxpool.ParseConfig(o.dbURL)
		if err != nil {
			return err
		}
		cfg.MaxConns = int32(o.workers + 16)
		pool, err = pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			return err
		}
		defer pool.Close()
		if err := pgstore.Migrate(ctx, pool); err != nil {
			return err
		}
		if o.reset {
			if _, err := pool.Exec(ctx, `TRUNCATE entries, transfers, accounts`); err != nil {
				return err
			}
			if err := pgstore.EnsureSystemAccounts(ctx, pool); err != nil {
				return err
			}
		}
		store = pgstore.New(pool)
	} else {
		fmt.Println("no -database-url, using the in-memory store")
		store = ledger.NewMemStore()
	}

	rng := rand.New(rand.NewPCG(o.seed, o.seed*7919))
	runID := fmt.Sprintf("%x", time.Now().UnixNano())

	// accounts, each funded with 1M USDC so nothing gets rejected and the
	// expected balances can be computed from the plan alone
	const seedBalance = 1_000_000
	accts := make([]string, o.accounts)
	expected := map[string]money.Amount{}
	for i := range accts {
		a, err := store.CreateAccount(ctx, ledger.NewAccount{ID: fmt.Sprintf("acct_replay_%s_%04d", runID, i)})
		if err != nil {
			return err
		}
		accts[i] = a.ID
		if _, _, err := store.CreateTransfer(ctx, ledger.TransferRequest{
			IdempotencyKey: "seed-" + a.ID, Kind: ledger.KindInternal,
			FromAccount: ledger.AccountOnchainIn, ToAccount: a.ID, Amount: money.USDC(seedBalance),
		}); err != nil {
			return err
		}
		expected[a.ID] = money.USDC(seedBalance)
	}

	// the plan
	ops := make([]op, o.n)
	var withdrawals, releases int
	var settledTotal money.Amount
	for i := range ops {
		from := accts[rng.IntN(len(accts))]
		amt := money.FromMicros(10_000 + rng.Int64N(50_000_000)) // 0.01 .. 50 USDC
		op := op{sends: 1}
		key := fmt.Sprintf("replay-%s-%d", runID, i)
		if rng.Float64() < o.withdrawRate {
			withdrawals++
			op.req = ledger.TransferRequest{IdempotencyKey: key, Kind: ledger.KindWithdrawal, FromAccount: from,
				Destination: fmt.Sprintf("0x%040x", rng.Uint64()), Amount: amt}
			op.release = rng.Float64() < o.releaseRate
			op.settleCalls = 1 + rng.IntN(3)
			if op.release {
				releases++
			} else {
				expected[from] -= amt
				settledTotal += amt
			}
		} else {
			to := from
			for to == from {
				to = accts[rng.IntN(len(accts))]
			}
			op.req = ledger.TransferRequest{IdempotencyKey: key, Kind: ledger.KindInternal, FromAccount: from, ToAccount: to, Amount: amt}
			expected[from] -= amt
			expected[to] += amt
		}
		if rng.Float64() < o.dupRate {
			op.sends += 1 + rng.IntN(4)
			op.race = rng.Float64() < o.raceRate
		}
		op.cancelFirst = rng.Float64() < o.cancelRate
		ops[i] = op
	}

	fmt.Printf("replaying %d transfers (%d internal, %d withdrawals, %d of them failing) across %d accounts with %d workers\n",
		o.n, o.n-withdrawals, withdrawals, releases, o.accounts, o.workers)

	var st stats
	var firstErr error
	var errOnce sync.Once
	jobs := make(chan int)
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < o.workers; w++ {
		wg.Add(1)
		go func(seed uint64) {
			defer wg.Done()
			r := rand.New(rand.NewPCG(seed, seed^0xabcdef))
			for i := range jobs {
				if err := execute(ctx, store, ops[i], r, &st); err != nil {
					errOnce.Do(func() { firstErr = fmt.Errorf("transfer %d: %w", i, err) })
				}
			}
		}(o.seed + uint64(w) + 1)
	}
	for i := range ops {
		jobs <- i
		if (i+1)%10_000 == 0 {
			fmt.Printf("  %6d/%d  %.0f transfers/s\n", i+1, o.n, float64(i+1)/time.Since(start).Seconds())
		}
	}
	close(jobs)
	wg.Wait()
	elapsed := time.Since(start)
	if firstErr != nil {
		return firstErr
	}

	fmt.Println()
	fmt.Printf("sent %d CreateTransfer calls for %d transfers in %s (%.0f transfers/s)\n",
		st.requests.Load(), o.n, elapsed.Round(time.Millisecond), float64(o.n)/elapsed.Seconds())
	fmt.Printf("  %d duplicate sends (%d transfers raced concurrently), %d first attempts cancelled mid-flight (%d of those had already committed)\n",
		st.duplicates.Load(), st.raced.Load(), st.cancelled.Load(), st.cancelledCommitted.Load())
	fmt.Printf("  %d settle/release/mark-submitted calls, %d transient errors retried\n\n",
		st.lifecycleCalls.Load(), st.transientErrs.Load())

	ok := true
	check := func(name string, pass bool, detail string) {
		mark := "ok  "
		if !pass {
			mark = "FAIL"
			ok = false
		}
		fmt.Printf("  [%s] %-38s %s\n", mark, name, detail)
	}

	fmt.Println("checks")
	// 1. balances vs plan
	drift := 0
	var driftAbs money.Amount
	for id, want := range expected {
		a, err := store.GetAccount(ctx, id)
		if err != nil {
			return err
		}
		if a.Balance != want {
			drift++
			d := a.Balance - want
			if d < 0 {
				d = -d
			}
			driftAbs += d
		}
	}
	check("balances match the plan", drift == 0, fmt.Sprintf("%d accounts drifted, total %s USDC", drift, driftAbs))

	settlement, _ := store.GetAccount(ctx, ledger.AccountSettlement)
	check("nothing left in settlement", settlement.Balance == 0 || !o.reset, "sys_settlement = "+settlement.Balance.String())
	if o.reset || pool == nil {
		out, _ := store.GetAccount(ctx, ledger.AccountOnchainOut)
		check("on-chain out == settled withdrawals", out.Balance == settledTotal, fmt.Sprintf("%s vs %s", out.Balance, settledTotal))
	}

	if pool != nil {
		if err := sqlChecks(ctx, pool, runID, o.n, check); err != nil {
			return err
		}
	}
	fmt.Println()
	if !ok {
		return errors.New("checks failed")
	}
	fmt.Println("all checks passed")
	return nil
}

// execute sends one planned transfer the way a flaky client would.
func execute(ctx context.Context, s ledger.Store, o op, r *rand.Rand, st *stats) error {
	if o.cancelFirst {
		st.cancelled.Add(1)
		st.requests.Add(1)
		cctx, cancel := context.WithTimeout(ctx, time.Duration(r.IntN(2000))*time.Microsecond)
		_, replayed, err := s.CreateTransfer(cctx, o.req)
		cancel()
		if err == nil && !replayed {
			st.cancelledCommitted.Add(1)
			st.fresh.Add(1)
		}
	}

	send := func() (ledger.Transfer, error) {
		for attempt := 0; ; attempt++ {
			st.requests.Add(1)
			t, replayed, err := s.CreateTransfer(ctx, o.req)
			if err == nil {
				if !replayed {
					st.fresh.Add(1)
				}
				return t, nil
			}
			if attempt >= 10 || errors.Is(err, ledger.ErrIdempotencyConflict) || errors.Is(err, ledger.ErrInvalidRequest) {
				return t, err
			}
			st.transientErrs.Add(1)
			time.Sleep(time.Duration(attempt+1) * time.Millisecond)
		}
	}

	st.duplicates.Add(int64(o.sends - 1))
	ids := make([]string, o.sends)
	errs := make([]error, o.sends)
	if o.race && o.sends > 1 {
		st.raced.Add(1)
		var wg sync.WaitGroup
		for i := 0; i < o.sends; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				t, err := send()
				ids[i], errs[i] = t.ID, err
			}(i)
		}
		wg.Wait()
	} else {
		for i := 0; i < o.sends; i++ {
			t, err := send()
			ids[i], errs[i] = t.ID, err
		}
	}
	for i := range ids {
		if errs[i] != nil {
			return errs[i]
		}
		if ids[i] != ids[0] {
			return fmt.Errorf("same key returned two transfers: %s and %s", ids[0], ids[i])
		}
	}
	if o.req.Kind != ledger.KindWithdrawal {
		return nil
	}

	// settlement side, with the activity retries a workflow would do
	id := ids[0]
	sum := sha256.Sum256([]byte(id))
	hash := "0x" + hex.EncodeToString(sum[:])
	calls := func(fn func() error) error {
		var wg sync.WaitGroup
		errs := make([]error, o.settleCalls)
		for i := 0; i < o.settleCalls; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				st.lifecycleCalls.Add(1)
				errs[i] = retryTransient(st, fn)
			}(i)
		}
		wg.Wait()
		return errors.Join(errs...)
	}
	if o.release {
		return calls(func() error { _, err := s.Release(ctx, id, "replay: simulated revert"); return err })
	}
	if err := calls(func() error { _, err := s.MarkSubmitted(ctx, id, hash); return err }); err != nil {
		return err
	}
	return calls(func() error { _, err := s.Settle(ctx, id); return err })
}

func retryTransient(st *stats, fn func() error) error {
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		if err = fn(); err == nil || errors.Is(err, ledger.ErrInvalidState) || errors.Is(err, ledger.ErrNotFound) {
			return err
		}
		st.transientErrs.Add(1)
		time.Sleep(time.Duration(attempt+1) * time.Millisecond)
	}
	return err
}

func sqlChecks(ctx context.Context, pool *pgxpool.Pool, runID string, n int, check func(string, bool, string)) error {
	var stored int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM transfers WHERE idempotency_key LIKE $1`,
		"replay-"+runID+"-%").Scan(&stored); err != nil {
		return err
	}
	check("every key stored exactly once", stored == n, fmt.Sprintf("%d/%d", stored, n))

	// the headline number: a transfer phase debited more than once
	var dupDebits int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM (
			SELECT transfer_id, phase FROM entries WHERE direction = 'debit'
			GROUP BY transfer_id, phase HAVING count(*) > 1) d`).Scan(&dupDebits); err != nil {
		return err
	}
	check("duplicate debits", dupDebits == 0, fmt.Sprintf("%d", dupDebits))

	// internal: 1 debit (post). withdrawal: 2 (hold + settle/release)
	var wrongShape int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM transfers t
		LEFT JOIN (SELECT transfer_id, count(*) AS debits FROM entries WHERE direction = 'debit' GROUP BY transfer_id) e
			ON e.transfer_id = t.id
		WHERE t.idempotency_key LIKE $1
		  AND coalesce(e.debits, 0) <> CASE WHEN t.kind = 'internal' THEN 1 ELSE 2 END`,
		"replay-"+runID+"-%").Scan(&wrongShape); err != nil {
		return err
	}
	check("debits per transfer as expected", wrongShape == 0, fmt.Sprintf("%d transfers off", wrongShape))

	var stuck int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM transfers WHERE idempotency_key LIKE $1 AND status NOT IN ('posted', 'settled', 'failed')`,
		"replay-"+runID+"-%").Scan(&stuck); err != nil {
		return err
	}
	check("no transfers left in flight", stuck == 0, fmt.Sprintf("%d", stuck))

	// balance column vs entries, for every account in the database
	var drift int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM accounts a
		LEFT JOIN (
			SELECT account_id, sum(CASE WHEN direction = 'credit' THEN amount ELSE -amount END) AS net
			FROM entries GROUP BY account_id) x ON x.account_id = a.id
		WHERE a.balance <> coalesce(x.net, 0)`).Scan(&drift); err != nil {
		return err
	}
	check("balances match entries (drift)", drift == 0, fmt.Sprintf("%d accounts", drift))

	var sum int64
	if err := pool.QueryRow(ctx, `SELECT coalesce(sum(balance), 0) FROM accounts`).Scan(&sum); err != nil {
		return err
	}
	check("ledger sums to zero", sum == 0, fmt.Sprintf("sum = %s", money.FromMicros(sum)))
	return nil
}
