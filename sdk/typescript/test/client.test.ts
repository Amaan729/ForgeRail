import { test } from "node:test";
import assert from "node:assert/strict";

import { ForgeRailClient, ForgeRailError, type Transfer } from "../src/index.js";

type Handler = (url: string, init: RequestInit) => Response | Promise<Response>;

/** fake fetch that records every call and answers from a script */
function fakeFetch(...handlers: Handler[]) {
  const calls: { url: string; init: RequestInit }[] = [];
  let i = 0;
  const fn = (async (input: string | URL | Request, init?: RequestInit) => {
    const url = String(input);
    calls.push({ url, init: init ?? {} });
    const h = handlers[Math.min(i++, handlers.length - 1)]!;
    return h(url, init ?? {});
  }) as typeof fetch;
  return { fn, calls };
}

const json = (status: number, body: unknown, headers: Record<string, string> = {}) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json", ...headers } });

const transfer = (over: Partial<Transfer> = {}): Transfer => ({
  id: "tr_1",
  idempotency_key: "k",
  kind: "internal",
  from_account_id: "acct_a",
  to_account_id: "acct_b",
  amount: "1.000000",
  status: "posted",
  created_at: "2026-09-01T00:00:00Z",
  updated_at: "2026-09-01T00:00:00Z",
  ...over,
});

const header = (init: RequestInit, name: string) => (init.headers as Record<string, string>)[name];

const client = (f: typeof fetch) =>
  new ForgeRailClient({ baseUrl: "http://api.test/", fetch: f, retryBaseDelayMs: 1, maxRetries: 3 });

const req = { kind: "internal", from_account_id: "acct_a", to_account_id: "acct_b", amount: "1" } as const;

test("retries 5xx with the same idempotency key", async () => {
  const { fn, calls } = fakeFetch(
    () => json(503, { error: { code: "settlement_unavailable", message: "try again" } }),
    () => json(500, { error: { code: "internal", message: "oops" } }),
    () => json(201, transfer()),
  );
  const res = await client(fn).createTransfer(req, { idempotencyKey: "my-key" });
  assert.equal(res.transfer.id, "tr_1");
  assert.equal(res.replayed, false);
  assert.equal(calls.length, 3);
  for (const c of calls) {
    assert.equal(header(c.init, "Idempotency-Key"), "my-key");
    assert.equal(c.url, "http://api.test/v1/transfers");
  }
});

test("generates one key per createTransfer call and keeps it across retries", async () => {
  const { fn, calls } = fakeFetch(
    () => {
      throw new TypeError("fetch failed");
    },
    () => json(201, transfer()),
  );
  await client(fn).createTransfer(req);
  const keys = calls.map((c) => header(c.init, "Idempotency-Key"));
  assert.equal(keys.length, 2);
  assert.match(keys[0]!, /^[0-9a-f-]{36}$/);
  assert.equal(keys[0], keys[1]);
});

test("reports replays", async () => {
  const { fn } = fakeFetch(() => json(200, transfer(), { "Idempotent-Replayed": "true" }));
  const res = await client(fn).createTransfer(req, { idempotencyKey: "k" });
  assert.equal(res.replayed, true);
});

test("does not retry 4xx and surfaces the error code", async () => {
  const { fn, calls } = fakeFetch(() =>
    json(409, { error: { code: "idempotency_conflict", message: "different body" } }),
  );
  await assert.rejects(client(fn).createTransfer(req, { idempotencyKey: "k" }), (err: unknown) => {
    assert.ok(err instanceof ForgeRailError);
    assert.equal(err.status, 409);
    assert.equal(err.code, "idempotency_conflict");
    assert.equal(err.retryable, false);
    return true;
  });
  assert.equal(calls.length, 1);
});

test("gives up after maxRetries", async () => {
  const { fn, calls } = fakeFetch(() => json(503, { error: { code: "internal", message: "down" } }));
  await assert.rejects(client(fn).getTransfer("tr_1"), ForgeRailError);
  assert.equal(calls.length, 4); // 1 + 3 retries
});

test("createAccount is never retried (not idempotent)", async () => {
  const { fn, calls } = fakeFetch(() => json(503, { error: { code: "internal", message: "down" } }));
  await assert.rejects(client(fn).createAccount("alice"), ForgeRailError);
  assert.equal(calls.length, 1);
});

test("waitForTransfer polls until a terminal status", async () => {
  const { fn, calls } = fakeFetch(
    () => json(200, transfer({ kind: "withdrawal", status: "pending" })),
    () => json(200, transfer({ kind: "withdrawal", status: "submitted", tx_hash: "0xabc" })),
    () => json(200, transfer({ kind: "withdrawal", status: "settled", tx_hash: "0xabc" })),
  );
  const t = await client(fn).waitForTransfer("tr_1", { intervalMs: 1 });
  assert.equal(t.status, "settled");
  assert.equal(calls.length, 3);
});

test("encodes path parameters", async () => {
  const { fn, calls } = fakeFetch(() => json(200, { entries: [] }));
  await client(fn).listEntries("acct/../x", { limit: 5 });
  assert.equal(calls[0]!.url, "http://api.test/v1/accounts/acct%2F..%2Fx/entries?limit=5");
});
