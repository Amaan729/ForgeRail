# ForgeRail

Stablecoin (USDC) settlement platform written in Go.

Work in progress. The plan:

- idempotent double-entry ledger for USDC balances (PostgreSQL)
- gRPC + REST APIs
- Temporal workflows that settle withdrawals on Base
- TypeScript SDK
