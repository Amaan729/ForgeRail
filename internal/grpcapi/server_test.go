package grpcapi

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	forgerailv1 "github.com/Amaan729/ForgeRail/gen/forgerail/v1"
	"github.com/Amaan729/ForgeRail/internal/chain"
	"github.com/Amaan729/ForgeRail/internal/ledger"
	"github.com/Amaan729/ForgeRail/internal/service"
	"github.com/Amaan729/ForgeRail/internal/settlement"
)

func newClient(t *testing.T, allowSystem bool) forgerailv1.LedgerServiceClient {
	t.Helper()
	store := ledger.NewMemStore()
	runner := &settlement.LocalRunner{
		Acts:    &settlement.Activities{Store: store, Chain: chain.NewFake(chain.FakeConfig{}), PollInterval: time.Millisecond},
		Backoff: time.Millisecond,
	}
	svc := &service.Service{Store: store, Starter: runner, AllowSystemTransfers: allowSystem}

	lis := bufconn.Listen(1 << 20)
	g := grpc.NewServer()
	Register(g, svc)
	go g.Serve(lis)
	t.Cleanup(func() {
		g.Stop()
		runner.Wait()
	})

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return forgerailv1.NewLedgerServiceClient(conn)
}

func wantCode(t *testing.T, err error, code codes.Code) {
	t.Helper()
	if status.Code(err) != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestTransfersOverGRPC(t *testing.T) {
	c := newClient(t, true)
	ctx := context.Background()

	alice, err := c.CreateAccount(ctx, &forgerailv1.CreateAccountRequest{Name: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	bob, _ := c.CreateAccount(ctx, &forgerailv1.CreateAccountRequest{Name: "bob"})
	a, b := alice.Account.Id, bob.Account.Id

	fund := &forgerailv1.CreateTransferRequest{
		IdempotencyKey: "fund-alice", Kind: forgerailv1.TransferKind_TRANSFER_KIND_INTERNAL,
		FromAccountId: ledger.AccountOnchainIn, ToAccountId: a, AmountMicros: 50_000_000,
	}
	if _, err := c.CreateTransfer(ctx, fund); err != nil {
		t.Fatal(err)
	}

	pay := &forgerailv1.CreateTransferRequest{
		IdempotencyKey: "pay-1", Kind: forgerailv1.TransferKind_TRANSFER_KIND_INTERNAL,
		FromAccountId: a, ToAccountId: b, AmountMicros: 12_340_000, Memo: "pizza",
	}
	first, err := c.CreateTransfer(ctx, pay)
	if err != nil {
		t.Fatal(err)
	}
	if first.Replayed || first.Transfer.Status != forgerailv1.TransferStatus_TRANSFER_STATUS_POSTED {
		t.Fatalf("unexpected response %v", first)
	}
	again, err := c.CreateTransfer(ctx, pay)
	if err != nil || !again.Replayed || again.Transfer.Id != first.Transfer.Id {
		t.Fatalf("replay: %v %v", again, err)
	}

	got, _ := c.GetAccount(ctx, &forgerailv1.GetAccountRequest{Id: a})
	if got.Account.BalanceMicros != 50_000_000-12_340_000 {
		t.Errorf("alice balance %d", got.Account.BalanceMicros)
	}

	pay.AmountMicros = 1
	_, err = c.CreateTransfer(ctx, pay)
	wantCode(t, err, codes.FailedPrecondition)

	broke, err := c.CreateTransfer(ctx, &forgerailv1.CreateTransferRequest{
		IdempotencyKey: "too-much", Kind: forgerailv1.TransferKind_TRANSFER_KIND_INTERNAL,
		FromAccountId: b, ToAccountId: a, AmountMicros: 999_000_000,
	})
	if err != nil || broke.Transfer.Status != forgerailv1.TransferStatus_TRANSFER_STATUS_REJECTED ||
		broke.Transfer.FailureReason != ledger.FailureInsufficientFunds {
		t.Fatalf("insufficient funds: %v %v", broke, err)
	}

	entries, err := c.ListEntries(ctx, &forgerailv1.ListEntriesRequest{AccountId: b})
	if err != nil || len(entries.Entries) != 1 || entries.Entries[0].Direction != forgerailv1.EntryDirection_ENTRY_DIRECTION_CREDIT {
		t.Fatalf("entries: %v %v", entries, err)
	}
}

func TestWithdrawalSettlesOverGRPC(t *testing.T) {
	c := newClient(t, true)
	ctx := context.Background()
	acct, _ := c.CreateAccount(ctx, &forgerailv1.CreateAccountRequest{Name: "carol"})
	id := acct.Account.Id
	c.CreateTransfer(ctx, &forgerailv1.CreateTransferRequest{
		IdempotencyKey: "fund", Kind: forgerailv1.TransferKind_TRANSFER_KIND_INTERNAL,
		FromAccountId: ledger.AccountOnchainIn, ToAccountId: id, AmountMicros: 10_000_000,
	})
	resp, err := c.CreateTransfer(ctx, &forgerailv1.CreateTransferRequest{
		IdempotencyKey: "wd", Kind: forgerailv1.TransferKind_TRANSFER_KIND_WITHDRAWAL,
		FromAccountId: id, DestinationAddress: "0x" + strings.Repeat("ee", 20), AmountMicros: 4_000_000,
	})
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := c.GetTransfer(ctx, &forgerailv1.GetTransferRequest{Id: resp.Transfer.Id})
		if err != nil {
			t.Fatal(err)
		}
		if got.Transfer.Status == forgerailv1.TransferStatus_TRANSFER_STATUS_SETTLED {
			if got.Transfer.TxHash == "" {
				t.Error("settled without a tx hash")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("withdrawal stuck in %s", got.Transfer.Status)
		}
		time.Sleep(5 * time.Millisecond)
	}
	bal, _ := c.GetAccount(ctx, &forgerailv1.GetAccountRequest{Id: id})
	if bal.Account.BalanceMicros != 6_000_000 {
		t.Errorf("balance %d", bal.Account.BalanceMicros)
	}
}

func TestErrorCodes(t *testing.T) {
	c := newClient(t, false)
	ctx := context.Background()
	_, err := c.GetAccount(ctx, &forgerailv1.GetAccountRequest{Id: "acct_nope"})
	wantCode(t, err, codes.NotFound)

	_, err = c.GetAccount(ctx, &forgerailv1.GetAccountRequest{})
	wantCode(t, err, codes.InvalidArgument)

	_, err = c.CreateTransfer(ctx, &forgerailv1.CreateTransferRequest{IdempotencyKey: "k", AmountMicros: 1, FromAccountId: "x", ToAccountId: "y"})
	wantCode(t, err, codes.InvalidArgument) // kind unspecified

	acct, _ := c.CreateAccount(ctx, &forgerailv1.CreateAccountRequest{Name: "mallory"})
	_, err = c.CreateTransfer(ctx, &forgerailv1.CreateTransferRequest{
		IdempotencyKey: "mint", Kind: forgerailv1.TransferKind_TRANSFER_KIND_INTERNAL,
		FromAccountId: ledger.AccountOnchainIn, ToAccountId: acct.Account.Id, AmountMicros: 1_000_000_000,
	})
	wantCode(t, err, codes.PermissionDenied)
}
