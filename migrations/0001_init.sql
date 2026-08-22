-- ForgeRail initial schema.
--
-- Balances are stored in micro-USDC (bigint). accounts.balance is a running
-- total that is updated in the same transaction as the entries it summarises,
-- so it can always be rebuilt from the entries table (the replay tool checks
-- exactly that).

CREATE TABLE accounts (
    id             text        PRIMARY KEY,
    name           text        NOT NULL DEFAULT '',
    balance        bigint      NOT NULL DEFAULT 0,
    allow_negative boolean     NOT NULL DEFAULT false,
    created_at     timestamptz NOT NULL DEFAULT now(),
    -- last line of defence if the application ever gets the check wrong
    CONSTRAINT accounts_no_overdraft CHECK (allow_negative OR balance >= 0)
);

CREATE TABLE transfers (
    id              text        PRIMARY KEY,
    idempotency_key text        NOT NULL,
    request_hash    text        NOT NULL,
    kind            text        NOT NULL CHECK (kind IN ('internal', 'withdrawal')),
    from_account    text        NOT NULL REFERENCES accounts (id),
    to_account      text        REFERENCES accounts (id),
    destination     text,
    amount          bigint      NOT NULL CHECK (amount > 0),
    memo            text        NOT NULL DEFAULT '',
    status          text        NOT NULL CHECK (status IN ('posted', 'pending', 'submitted', 'settled', 'failed', 'rejected')),
    failure_reason  text        NOT NULL DEFAULT '',
    tx_hash         text        NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT transfers_idempotency_key_uniq UNIQUE (idempotency_key)
);

-- the settlement worker and ops queries look for in-flight withdrawals
CREATE INDEX transfers_in_flight_idx ON transfers (created_at)
    WHERE status IN ('pending', 'submitted');

CREATE TABLE entries (
    id          bigserial   PRIMARY KEY,
    transfer_id text        NOT NULL REFERENCES transfers (id),
    account_id  text        NOT NULL REFERENCES accounts (id),
    direction   text        NOT NULL CHECK (direction IN ('debit', 'credit')),
    amount      bigint      NOT NULL CHECK (amount > 0),
    phase       text        NOT NULL CHECK (phase IN ('post', 'hold', 'settle', 'release')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    -- one debit and one credit per (transfer, phase). Re-posting a phase is a no-op.
    CONSTRAINT entries_once_per_phase UNIQUE (transfer_id, phase, direction)
);

CREATE INDEX entries_account_idx ON entries (account_id, id DESC);
