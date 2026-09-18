package chain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/Amaan729/ForgeRail/internal/money"
)

// ErrFakeUnavailable is the transient error the fake chain injects.
var ErrFakeUnavailable = errors.New("fake chain: rpc unavailable")

// FakeConfig controls failure injection. Zero value = a perfect chain.
type FakeConfig struct {
	// BroadcastFailRate is the chance a Broadcast call returns a transient
	// error. Half of those failures happen *after* the tx was accepted, which
	// is the nasty case (caller thinks it failed, chain has it).
	BroadcastFailRate float64
	// RevertRate is the chance an accepted tx ends up reverted.
	RevertRate float64
	// ConfirmAfter is how long after broadcast a tx reports as confirmed.
	ConfirmAfter time.Duration
	Seed         uint64
}

type fakeTx struct {
	ref       string
	to        string
	amount    money.Amount
	accepted  time.Time
	broadcast bool
	reverts   bool
}

// Fake is an in-memory chain used by tests, the replay tool and dev mode.
type Fake struct {
	cfg FakeConfig

	mu       sync.Mutex
	rng      *rand.Rand
	byRef    map[string]string
	txs      map[string]*fakeTx
	accepted int
	block    uint64
}

var _ Client = (*Fake)(nil)

func NewFake(cfg FakeConfig) *Fake {
	return &Fake{
		cfg:   cfg,
		rng:   rand.New(rand.NewPCG(cfg.Seed, cfg.Seed^0x9e3779b97f4a7c15)),
		byRef: map[string]string{},
		txs:   map[string]*fakeTx{},
	}
}

func (f *Fake) PrepareUSDCTransfer(_ context.Context, ref, to string, amount money.Amount) (SignedTx, error) {
	if len(to) != 42 {
		return SignedTx{}, fmt.Errorf("%w: bad address %q", ErrPermanent, to)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// Signing the same transfer twice gives the same tx, like reusing a nonce.
	if h, ok := f.byRef[ref]; ok {
		return SignedTx{Hash: h, Raw: []byte(ref)}, nil
	}
	sum := sha256.Sum256([]byte("forgerail/" + ref))
	h := "0x" + hex.EncodeToString(sum[:])
	f.byRef[ref] = h
	f.txs[h] = &fakeTx{ref: ref, to: to, amount: amount, reverts: f.rng.Float64() < f.cfg.RevertRate}
	return SignedTx{Hash: h, Raw: []byte(ref)}, nil
}

func (f *Fake) Broadcast(ctx context.Context, tx SignedTx) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.txs[tx.Hash]
	if !ok {
		return fmt.Errorf("%w: unknown tx %s", ErrPermanent, tx.Hash)
	}
	fail := f.rng.Float64() < f.cfg.BroadcastFailRate
	failAfter := fail && f.rng.IntN(2) == 0
	if fail && !failAfter {
		return ErrFakeUnavailable
	}
	if !t.broadcast {
		t.broadcast = true
		t.accepted = time.Now()
		f.accepted++
		f.block++
	}
	if failAfter {
		return ErrFakeUnavailable
	}
	return nil
}

func (f *Fake) Receipt(ctx context.Context, hash string) (Receipt, error) {
	if err := ctx.Err(); err != nil {
		return Receipt{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.txs[hash]
	if !ok || !t.broadcast {
		return Receipt{TxHash: hash, Status: TxNotFound}, nil
	}
	if time.Since(t.accepted) < f.cfg.ConfirmAfter {
		return Receipt{TxHash: hash, Status: TxPending}, nil
	}
	st := TxConfirmed
	if t.reverts {
		st = TxReverted
	}
	return Receipt{TxHash: hash, Status: st, BlockNumber: f.block, Confirmations: 3}, nil
}

// Broadcasts returns how many distinct txs reached the chain. The replay
// tool compares this with the number of settled withdrawals.
func (f *Fake) Broadcasts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.accepted
}
