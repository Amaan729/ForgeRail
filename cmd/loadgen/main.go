// Command loadgen drives the REST API at a fixed request rate (open loop, so
// a slow server doesn't slow the arrival rate down) and retries failures
// with the same idempotency key, like the SDK does. When it's done it waits
// for withdrawals to finish and checks every balance against what the
// responses said happened.
//
// Start the server with -dev (so loadgen can fund accounts), then:
//
//	go run ./cmd/loadgen -url http://localhost:8080 -rps 400 -duration 60s
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Amaan729/ForgeRail/internal/money"
)

type options struct {
	url          string
	rps          int
	duration     time.Duration
	accounts     int
	withdrawRate float64
	timeout      time.Duration
	retries      int
	drain        time.Duration
}

func main() {
	var o options
	flag.StringVar(&o.url, "url", "http://localhost:8080", "ForgeRail REST base URL")
	flag.IntVar(&o.rps, "rps", 400, "transfer requests per second")
	flag.DurationVar(&o.duration, "duration", 60*time.Second, "how long to generate load")
	flag.IntVar(&o.accounts, "accounts", 200, "accounts to spread load over")
	flag.Float64Var(&o.withdrawRate, "withdraw-rate", 0.2, "fraction of requests that are withdrawals")
	flag.DurationVar(&o.timeout, "timeout", 3*time.Second, "per-attempt timeout")
	flag.IntVar(&o.retries, "retries", 6, "retries per request (same idempotency key)")
	flag.DurationVar(&o.drain, "drain", 60*time.Second, "max time to wait for withdrawals to finish")
	flag.Parse()

	if err := run(context.Background(), o); err != nil {
		fmt.Fprintln(os.Stderr, "loadgen:", err)
		os.Exit(1)
	}
}

type transfer struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	FromAccountID string `json:"from_account_id"`
	ToAccountID   string `json:"to_account_id"`
	Amount        string `json:"amount"`
	Status        string `json:"status"`
}

type client struct {
	base string
	http *http.Client
}

type apiError struct {
	status int
	code   string
}

func (e *apiError) Error() string { return fmt.Sprintf("%d %s", e.status, e.code) }

func (c *client) do(ctx context.Context, method, path, key string, body, out any) (int, error) {
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, &buf)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e struct {
			Error struct{ Code string } `json:"error"`
		}
		json.Unmarshal(data, &e)
		return resp.StatusCode, &apiError{resp.StatusCode, e.Error.Code}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

type counters struct {
	attempts, created, replayed, retried, failed atomic.Int64
	status5xx, status4xx, netErrs                atomic.Int64
}

type result struct {
	t       transfer
	latency time.Duration
}

func run(ctx context.Context, o options) error {
	c := &client{
		base: strings.TrimRight(o.url, "/"),
		http: &http.Client{Transport: &http.Transport{MaxIdleConns: 1024, MaxIdleConnsPerHost: 1024}},
	}

	// setup: accounts funded with 100k USDC each
	runID := fmt.Sprintf("%x", time.Now().UnixNano())
	fmt.Printf("creating and funding %d accounts...\n", o.accounts)
	accts := make([]string, o.accounts)
	for i := range accts {
		var a struct{ ID string }
		if _, err := c.do(ctx, "POST", "/v1/accounts", "", map[string]string{"name": fmt.Sprintf("load-%s-%d", runID, i)}, &a); err != nil {
			return fmt.Errorf("create account (is the server up?): %w", err)
		}
		accts[i] = a.ID
		fund := map[string]string{"kind": "internal", "from_account_id": "sys_onchain_in", "to_account_id": a.ID, "amount": "100000"}
		if err := retryPost(ctx, c, o, "fund-"+a.ID, fund, nil, &counters{}); err != nil {
			return fmt.Errorf("fund account (server needs -dev): %w", err)
		}
	}
	start := map[string]money.Amount{}
	for _, id := range accts {
		start[id] = money.USDC(100_000)
	}

	var cnt counters
	var mu sync.Mutex
	var results []result
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4096)

	fmt.Printf("sending %d transfers/s for %s (%.0f%% withdrawals)...\n", o.rps, o.duration, o.withdrawRate*100)
	ticker := time.NewTicker(time.Second / time.Duration(o.rps))
	defer ticker.Stop()
	deadline := time.Now().Add(o.duration)
	began := time.Now()
	var sent int
	for time.Now().Before(deadline) {
		<-ticker.C
		sent++
		i := sent
		select {
		case sem <- struct{}{}:
		default:
			cnt.failed.Add(1) // client overloaded, count as a failure
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			from := accts[rand.IntN(len(accts))]
			amt := money.FromMicros(10_000 + rand.Int64N(20_000_000)).String()
			body := map[string]string{"from_account_id": from, "amount": amt}
			if rand.Float64() < o.withdrawRate {
				body["kind"] = "withdrawal"
				body["destination_address"] = fmt.Sprintf("0x%040x", rand.Uint64())
			} else {
				to := from
				for to == from {
					to = accts[rand.IntN(len(accts))]
				}
				body["kind"] = "internal"
				body["to_account_id"] = to
			}
			t0 := time.Now()
			var tr transfer
			if err := retryPost(ctx, c, o, fmt.Sprintf("load-%s-%d", runID, i), body, &tr, &cnt); err != nil {
				cnt.failed.Add(1)
				return
			}
			mu.Lock()
			results = append(results, result{t: tr, latency: time.Since(t0)})
			mu.Unlock()
		}()
	}
	wg.Wait()
	loadTime := time.Since(began)

	// drain withdrawals
	fmt.Println("waiting for withdrawals to settle...")
	final := make(map[string]transfer, len(results))
	var pending []string
	for _, r := range results {
		final[r.t.ID] = r.t
		if r.t.Kind == "withdrawal" && r.t.Status != "settled" && r.t.Status != "failed" && r.t.Status != "rejected" {
			pending = append(pending, r.t.ID)
		}
	}
	drainStart := time.Now()
	for len(pending) > 0 && time.Since(drainStart) < o.drain {
		var still []string
		for _, id := range pending {
			var t transfer
			if _, err := c.do(ctx, "GET", "/v1/transfers/"+id, "", nil, &t); err != nil {
				still = append(still, id)
				continue
			}
			final[id] = t
			if t.Status != "settled" && t.Status != "failed" {
				still = append(still, id)
			}
		}
		pending = still
		if len(pending) > 0 {
			time.Sleep(250 * time.Millisecond)
		}
	}

	// expected balances from what the API told us
	expected := start
	var posted, settled, failedWd, rejected int
	for _, t := range final {
		amt := money.MustParse(t.Amount)
		switch t.Status {
		case "posted":
			posted++
			expected[t.FromAccountID] -= amt
			expected[t.ToAccountID] += amt
		case "settled":
			settled++
			expected[t.FromAccountID] -= amt
		case "failed":
			failedWd++
		case "rejected":
			rejected++
		}
	}
	var drift int
	for id, want := range expected {
		var a struct{ Balance string }
		if _, err := c.do(ctx, "GET", "/v1/accounts/"+id, "", nil, &a); err != nil {
			return err
		}
		if money.MustParse(a.Balance) != want {
			drift++
		}
	}

	lat := make([]time.Duration, len(results))
	for i, r := range results {
		lat[i] = r.latency
	}
	slices.Sort(lat)
	pct := func(p float64) time.Duration {
		if len(lat) == 0 {
			return 0
		}
		return lat[min(len(lat)-1, int(float64(len(lat))*p))].Round(100 * time.Microsecond)
	}

	ok := len(results)
	fmt.Println()
	fmt.Printf("load:        %d requests in %s, %d succeeded = %.1f successful transfers/s (target %d)\n",
		sent, loadTime.Round(time.Millisecond), ok, float64(ok)/loadTime.Seconds(), o.rps)
	fmt.Printf("latency:     p50 %s  p95 %s  p99 %s  max %s  (end to end, including retries)\n",
		pct(0.50), pct(0.95), pct(0.99), pct(1))
	fmt.Printf("attempts:    %d total, %d retried, %d 5xx, %d 4xx, %d network errors\n",
		cnt.attempts.Load(), cnt.retried.Load(), cnt.status5xx.Load(), cnt.status4xx.Load(), cnt.netErrs.Load())
	fmt.Printf("replays:     %d retries hit a transfer that had already committed (lost response)\n", cnt.replayed.Load())
	fmt.Printf("gave up:     %d requests failed after %d retries\n", cnt.failed.Load(), o.retries)
	fmt.Printf("outcomes:    %d internal posted, %d withdrawals settled, %d withdrawals failed+released, %d rejected, %d still in flight\n",
		posted, settled, failedWd, rejected, len(pending))
	fmt.Printf("balances:    %d of %d accounts differ from what the API responses imply\n", drift, len(expected))

	if drift > 0 || len(pending) > 0 {
		return errors.New("consistency check failed")
	}
	return nil
}

// retryPost sends a POST /v1/transfers with a fixed idempotency key and
// retries network errors, timeouts and 5xx with backoff.
func retryPost(ctx context.Context, c *client, o options, key string, body map[string]string, out *transfer, cnt *counters) error {
	delay := 50 * time.Millisecond
	var err error
	for attempt := 0; attempt <= o.retries; attempt++ {
		if attempt > 0 {
			cnt.retried.Add(1)
			time.Sleep(delay/2 + time.Duration(rand.Int64N(int64(delay/2)+1)))
			delay = min(delay*2, 2*time.Second)
		}
		cnt.attempts.Add(1)
		actx, cancel := context.WithTimeout(ctx, o.timeout)
		var tr transfer
		var code int
		code, err = c.do(actx, "POST", "/v1/transfers", key, body, &tr)
		cancel()
		var apiErr *apiError
		switch {
		case err == nil:
			if code == http.StatusOK {
				// an earlier attempt committed but we never saw the answer
				cnt.replayed.Add(1)
			} else {
				cnt.created.Add(1)
			}
			if out != nil {
				*out = tr
			}
			return nil
		case errors.As(err, &apiErr) && apiErr.status >= 500:
			cnt.status5xx.Add(1)
		case errors.As(err, &apiErr):
			cnt.status4xx.Add(1)
			return err // 4xx: retrying won't help
		default:
			cnt.netErrs.Add(1)
		}
	}
	return err
}
