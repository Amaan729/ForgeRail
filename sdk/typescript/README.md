# @forgerail/sdk

Small TypeScript client for the ForgeRail REST API (`api/openapi.yaml`).
No dependencies, uses the global `fetch` (Node 20+, Deno, Bun, browsers).

```ts
import { ForgeRailClient } from "@forgerail/sdk";

const fr = new ForgeRailClient({ baseUrl: "http://localhost:8080" });

const alice = await fr.createAccount("alice");
const bob = await fr.createAccount("bob");

// internal transfer: posts immediately
const { transfer } = await fr.createTransfer({
  kind: "internal",
  from_account_id: alice.id,
  to_account_id: bob.id,
  amount: "12.50",
});

// withdrawal to Base: pending -> submitted -> settled | failed
const { transfer: wd } = await fr.createTransfer({
  kind: "withdrawal",
  from_account_id: bob.id,
  destination_address: "0x1111111111111111111111111111111111111111",
  amount: "5",
});
const done = await fr.waitForTransfer(wd.id);
console.log(done.status, done.tx_hash);
```

## Retries and idempotency

`createTransfer` sends an `Idempotency-Key` header. If you don't pass one it
generates a UUID **once per call** and reuses it on every retry, so a request
that timed out after the server already committed it can't move money twice.

Retried: network errors, timeouts, 429 and 5xx (including
`settlement_unavailable`, which is exactly the case where retrying with the
same key fixes things). Not retried: any other 4xx.

If your process might crash between attempts, generate and store the key
yourself and pass it in:

```ts
await fr.createTransfer(req, { idempotencyKey: order.id });
```

`createAccount` is not idempotent on the server, so it is never retried.

## Amounts

Amounts are decimal strings (`"12.500000"`), never JS numbers, so nothing
gets rounded through a float.

## Development

```sh
npm ci
npm test
```
