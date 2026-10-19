-- All money columns are BIGINT in USDT base units (1 USDT = 1_000_000).

CREATE TABLE partners (
    id           TEXT PRIMARY KEY,
    name         TEXT        NOT NULL,
    api_key_hash BYTEA       NOT NULL UNIQUE, -- sha256 of the static API key
    fee_bps      INTEGER     NOT NULL CHECK (fee_bps BETWEEN 0 AND 10000),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Monotonic index for deposit address derivation (BIP-32 child index in prod).
CREATE SEQUENCE deposit_address_index;

CREATE TABLE invoices (
    id               UUID PRIMARY KEY,
    partner_id       TEXT        NOT NULL REFERENCES partners (id),
    idempotency_key  TEXT        NOT NULL,
    request_hash     BYTEA       NOT NULL, -- detects reuse of a key with a different body
    external_id      TEXT,
    amount           BIGINT      NOT NULL CHECK (amount > 0),
    fee_bps          INTEGER     NOT NULL, -- partner rate snapshotted at creation
    deposit_address  TEXT        NOT NULL UNIQUE,
    address_index    BIGINT      NOT NULL UNIQUE,
    status           TEXT        NOT NULL,
    credited_amount  BIGINT      NOT NULL DEFAULT 0, -- gross amount of AML-cleared transfers
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (partner_id, idempotency_key)
);

-- Watcher cursor: the highest block whose transfers have been recorded.
-- A single row; locking it serialises all watcher instances.
CREATE TABLE chain_cursor (
    id           SMALLINT PRIMARY KEY CHECK (id = 1),
    block_number BIGINT NOT NULL
);

-- Hashes of scanned blocks, used to detect reorgs and find the common ancestor.
CREATE TABLE scanned_blocks (
    number      BIGINT PRIMARY KEY,
    hash        TEXT NOT NULL,
    parent_hash TEXT NOT NULL
);

-- One row per ERC-20 Transfer log to one of our deposit addresses.
CREATE TABLE transfers (
    id             UUID PRIMARY KEY,
    invoice_id     UUID        NOT NULL REFERENCES invoices (id),
    partner_id     TEXT        NOT NULL REFERENCES partners (id),
    tx_hash        TEXT        NOT NULL,
    log_index      INTEGER     NOT NULL,
    block_number   BIGINT      NOT NULL,
    block_hash     TEXT        NOT NULL,
    from_address   TEXT        NOT NULL,
    to_address     TEXT        NOT NULL,
    amount         BIGINT      NOT NULL CHECK (amount > 0),
    -- DETECTED -> CONFIRMED -> CREDITED | REJECTED, or DETECTED -> ORPHANED
    status         TEXT        NOT NULL,
    aml_attempts   INTEGER     NOT NULL DEFAULT 0,
    aml_next_at    TIMESTAMPTZ,          -- also acts as a lease while a worker holds it
    aml_last_error TEXT,
    aml_risk       TEXT,
    fee            BIGINT,
    fee_bps        INTEGER,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (block_hash, tx_hash, log_index)
);

-- The same on-chain log may appear in several blocks across reorgs, but only
-- one copy can be live. This is the last line of defence against double credit.
CREATE UNIQUE INDEX transfers_live_log ON transfers (tx_hash, log_index) WHERE status <> 'ORPHANED';
CREATE INDEX transfers_invoice ON transfers (invoice_id);
CREATE INDEX transfers_detected ON transfers (block_number) WHERE status = 'DETECTED';
CREATE INDEX transfers_aml_queue ON transfers (aml_next_at) WHERE status = 'CONFIRMED';

CREATE TABLE refunds (
    id          UUID PRIMARY KEY,
    transfer_id UUID        NOT NULL UNIQUE REFERENCES transfers (id), -- one refund per transfer, ever
    invoice_id  UUID        NOT NULL REFERENCES invoices (id),
    partner_id  TEXT        NOT NULL REFERENCES partners (id),
    amount      BIGINT      NOT NULL CHECK (amount > 0),
    to_address  TEXT,        -- NULL while waiting for the sender to provide one
    reason      TEXT        NOT NULL,
    status      TEXT        NOT NULL, -- NEEDS_ADDRESS | READY_TO_SEND
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX refunds_invoice ON refunds (invoice_id);

-- Double-entry ledger. Postings are signed: debit > 0, credit < 0.
CREATE TABLE ledger_accounts (
    id          BIGSERIAL PRIMARY KEY,
    code        TEXT NOT NULL UNIQUE, -- e.g. liab:partner:acme:available
    normal_side TEXT NOT NULL CHECK (normal_side IN ('debit', 'credit'))
);

CREATE TABLE ledger_entries (
    id              BIGSERIAL PRIMARY KEY,
    idempotency_key TEXT        NOT NULL UNIQUE, -- e.g. transfer:<id>:confirm
    kind            TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE ledger_postings (
    id         BIGSERIAL PRIMARY KEY,
    entry_id   BIGINT NOT NULL REFERENCES ledger_entries (id),
    account_id BIGINT NOT NULL REFERENCES ledger_accounts (id),
    amount     BIGINT NOT NULL CHECK (amount <> 0)
);
CREATE INDEX ledger_postings_account ON ledger_postings (account_id);
CREATE INDEX ledger_postings_entry ON ledger_postings (entry_id);

-- Every entry must balance. Checked at commit, after all its postings exist.
CREATE FUNCTION ledger_check_balanced() RETURNS trigger AS $$
BEGIN
    IF (SELECT COALESCE(SUM(amount), 0) FROM ledger_postings WHERE entry_id = NEW.entry_id) <> 0 THEN
        RAISE EXCEPTION 'ledger entry % is unbalanced', NEW.entry_id;
    END IF;
    RETURN NULL;
END
$$ LANGUAGE plpgsql;

CREATE CONSTRAINT TRIGGER ledger_postings_balanced
    AFTER INSERT ON ledger_postings
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_check_balanced();

-- The ledger is append-only: corrections are new entries, never edits.
CREATE FUNCTION ledger_forbid_change() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'ledger is append-only';
END
$$ LANGUAGE plpgsql;

CREATE TRIGGER ledger_postings_append_only
    BEFORE UPDATE OR DELETE ON ledger_postings
    FOR EACH ROW EXECUTE FUNCTION ledger_forbid_change();

CREATE TRIGGER ledger_entries_append_only
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_forbid_change();
