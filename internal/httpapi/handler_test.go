package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Amaan729/ForgeRail/internal/chain"
	"github.com/Amaan729/ForgeRail/internal/ledger"
	"github.com/Amaan729/ForgeRail/internal/service"
	"github.com/Amaan729/ForgeRail/internal/settlement"
)

type testAPI struct {
	t   *testing.T
	srv *httptest.Server
}

func newAPI(t *testing.T, allowSystem bool) *testAPI {
	t.Helper()
	store := ledger.NewMemStore()
	runner := &settlement.LocalRunner{
		Acts:    &settlement.Activities{Store: store, Chain: chain.NewFake(chain.FakeConfig{}), PollInterval: time.Millisecond},
		Backoff: time.Millisecond,
	}
	svc := &service.Service{Store: store, Starter: runner, AllowSystemTransfers: allowSystem}
	srv := httptest.NewServer(New(svc, nil))
	t.Cleanup(func() {
		srv.Close()
		runner.Wait()
	})
	return &testAPI{t: t, srv: srv}
}

// do sends a request and decodes the JSON response into out (if non-nil).
func (a *testAPI) do(method, path, key string, body any, out any) *http.Response {
	a.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if s, ok := body.(string); ok {
			buf.WriteString(s)
		} else {
			json.NewEncoder(&buf).Encode(body)
		}
	}
	req, _ := http.NewRequest(method, a.srv.URL+path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			a.t.Fatalf("%s %s: decode: %v", method, path, err)
		}
	}
	return resp
}

type errBody struct {
	Error struct{ Code, Message string } `json:"error"`
}

func (a *testAPI) account(name string) accountJSON {
	a.t.Helper()
	var acct accountJSON
	if resp := a.do("POST", "/v1/accounts", "", map[string]string{"name": name}, &acct); resp.StatusCode != 201 {
		a.t.Fatalf("create account: %d", resp.StatusCode)
	}
	return acct
}

func (a *testAPI) fund(id, amount string) {
	a.t.Helper()
	resp := a.do("POST", "/v1/transfers", "fund-"+id+"-"+amount, createTransferJSON{
		Kind: "internal", FromAccountID: ledger.AccountOnchainIn, ToAccountID: id, Amount: amount,
	}, nil)
	if resp.StatusCode != 201 {
		a.t.Fatalf("fund: %d", resp.StatusCode)
	}
}

func TestTransferFlow(t *testing.T) {
	api := newAPI(t, true)
	alice, bob := api.account("alice"), api.account("bob")
	api.fund(alice.ID, "100")

	req := createTransferJSON{Kind: "internal", FromAccountID: alice.ID, ToAccountID: bob.ID, Amount: "12.5", Memo: "lunch"}
	var first transferJSON
	resp := api.do("POST", "/v1/transfers", "pay-1", req, &first)
	if resp.StatusCode != 201 || first.Status != "posted" || first.Amount != "12.500000" {
		t.Fatalf("create: %d %+v", resp.StatusCode, first)
	}

	var again transferJSON
	resp = api.do("POST", "/v1/transfers", "pay-1", req, &again)
	if resp.StatusCode != 200 || resp.Header.Get("Idempotent-Replayed") != "true" || again.ID != first.ID {
		t.Fatalf("replay: %d %v %+v", resp.StatusCode, resp.Header, again)
	}

	var got accountJSON
	api.do("GET", "/v1/accounts/"+alice.ID, "", nil, &got)
	if got.Balance != "87.500000" {
		t.Errorf("alice balance %s", got.Balance)
	}

	var tr transferJSON
	if resp := api.do("GET", "/v1/transfers/"+first.ID, "", nil, &tr); resp.StatusCode != 200 || tr.Memo != "lunch" {
		t.Errorf("get transfer: %d %+v", resp.StatusCode, tr)
	}

	var entries struct{ Entries []entryJSON }
	api.do("GET", "/v1/accounts/"+bob.ID+"/entries?limit=10", "", nil, &entries)
	if len(entries.Entries) != 1 || entries.Entries[0].Direction != "credit" || entries.Entries[0].Amount != "12.500000" {
		t.Errorf("entries: %+v", entries)
	}
}

func TestErrors(t *testing.T) {
	api := newAPI(t, false)
	alice, bob := api.account("alice"), api.account("bob")

	cases := []struct {
		name       string
		method     string
		path       string
		key        string
		body       any
		wantStatus int
		wantCode   string
	}{
		{"missing key", "POST", "/v1/transfers", "", createTransferJSON{Kind: "internal"}, 400, "missing_idempotency_key"},
		{"bad json", "POST", "/v1/transfers", "k1", "{nope", 400, "invalid_request"},
		{"unknown field", "POST", "/v1/transfers", "k2", `{"kind":"internal","amount":"1","surprise":true}`, 400, "invalid_request"},
		{"bad amount", "POST", "/v1/transfers", "k3", createTransferJSON{Kind: "internal", FromAccountID: alice.ID, ToAccountID: bob.ID, Amount: "1.0000001"}, 400, "invalid_request"},
		{"bad kind", "POST", "/v1/transfers", "k4", createTransferJSON{Kind: "swap", FromAccountID: alice.ID, ToAccountID: bob.ID, Amount: "1"}, 400, "invalid_request"},
		{"minting", "POST", "/v1/transfers", "k5", createTransferJSON{Kind: "internal", FromAccountID: ledger.AccountOnchainIn, ToAccountID: bob.ID, Amount: "1"}, 403, "forbidden"},
		{"unknown account", "GET", "/v1/accounts/acct_nope", "", nil, 404, "not_found"},
		{"unknown transfer", "GET", "/v1/transfers/tr_nope", "", nil, 404, "not_found"},
		{"bad limit", "GET", "/v1/accounts/" + alice.ID + "/entries?limit=0", "", nil, 400, "invalid_request"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var e errBody
			resp := api.do(c.method, c.path, c.key, c.body, &e)
			if resp.StatusCode != c.wantStatus || e.Error.Code != c.wantCode {
				t.Fatalf("got %d %q (%s), want %d %q", resp.StatusCode, e.Error.Code, e.Error.Message, c.wantStatus, c.wantCode)
			}
		})
	}
}

func TestKeyReuseAndRejection(t *testing.T) {
	api := newAPI(t, true)
	alice, bob := api.account("alice"), api.account("bob")
	api.fund(alice.ID, "5")

	api.do("POST", "/v1/transfers", "k", createTransferJSON{Kind: "internal", FromAccountID: alice.ID, ToAccountID: bob.ID, Amount: "1"}, nil)
	var e errBody
	resp := api.do("POST", "/v1/transfers", "k", createTransferJSON{Kind: "internal", FromAccountID: alice.ID, ToAccountID: bob.ID, Amount: "2"}, &e)
	if resp.StatusCode != 409 || e.Error.Code != "idempotency_conflict" {
		t.Fatalf("key reuse: %d %+v", resp.StatusCode, e)
	}

	var rej transferJSON
	resp = api.do("POST", "/v1/transfers", "big", createTransferJSON{Kind: "internal", FromAccountID: alice.ID, ToAccountID: bob.ID, Amount: "500"}, &rej)
	if resp.StatusCode != 201 || rej.Status != "rejected" || rej.FailureReason != "insufficient_funds" {
		t.Fatalf("rejection: %d %+v", resp.StatusCode, rej)
	}
}

func TestWithdrawal(t *testing.T) {
	api := newAPI(t, true)
	alice := api.account("alice")
	api.fund(alice.ID, "30")

	var tr transferJSON
	resp := api.do("POST", "/v1/transfers", "wd-1", createTransferJSON{
		Kind: "withdrawal", FromAccountID: alice.ID, Destination: "0x" + strings.Repeat("ab", 20), Amount: "10",
	}, &tr)
	if resp.StatusCode != 201 || tr.Status != "pending" {
		t.Fatalf("create withdrawal: %d %+v", resp.StatusCode, tr)
	}

	deadline := time.Now().Add(5 * time.Second)
	for tr.Status != "settled" {
		if time.Now().After(deadline) {
			t.Fatalf("stuck in %s", tr.Status)
		}
		time.Sleep(5 * time.Millisecond)
		api.do("GET", "/v1/transfers/"+tr.ID, "", nil, &tr)
	}
	if tr.TxHash == "" {
		t.Error("no tx hash on settled withdrawal")
	}
	var acct accountJSON
	api.do("GET", "/v1/accounts/"+alice.ID, "", nil, &acct)
	if acct.Balance != "20.000000" {
		t.Errorf("balance %s", acct.Balance)
	}
}
