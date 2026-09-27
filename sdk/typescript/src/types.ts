// Types mirror api/openapi.yaml. Amounts are decimal strings ("12.500000")
// so nothing is ever rounded through a JS number.

export type Amount = string;

export type TransferKind = "internal" | "withdrawal";

export type TransferStatus =
  | "posted"
  | "pending"
  | "submitted"
  | "settled"
  | "failed"
  | "rejected";

export const TERMINAL_STATUSES: ReadonlySet<TransferStatus> = new Set([
  "posted",
  "settled",
  "failed",
  "rejected",
]);

export interface Account {
  id: string;
  name: string;
  balance: Amount;
  created_at: string;
}

export interface Transfer {
  id: string;
  idempotency_key: string;
  kind: TransferKind;
  from_account_id: string;
  to_account_id?: string;
  destination_address?: string;
  amount: Amount;
  memo?: string;
  status: TransferStatus;
  failure_reason?: string;
  tx_hash?: string;
  created_at: string;
  updated_at: string;
}

export interface Entry {
  id: number;
  transfer_id: string;
  account_id: string;
  direction: "debit" | "credit";
  amount: Amount;
  phase: "post" | "hold" | "settle" | "release";
  created_at: string;
}

export interface InternalTransferRequest {
  kind: "internal";
  from_account_id: string;
  to_account_id: string;
  amount: Amount;
  memo?: string;
}

export interface WithdrawalRequest {
  kind: "withdrawal";
  from_account_id: string;
  destination_address: string;
  amount: Amount;
  memo?: string;
}

export type CreateTransferRequest = InternalTransferRequest | WithdrawalRequest;

export type ErrorCode =
  | "invalid_request"
  | "missing_idempotency_key"
  | "not_found"
  | "forbidden"
  | "idempotency_conflict"
  | "invalid_state"
  | "settlement_unavailable"
  | "internal";
