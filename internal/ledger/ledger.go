// Package ledger is a double-entry ledger on top of PostgreSQL.
//
// Every movement of money is an entry with two or more postings that sum to
// zero (debit positive, credit negative). Entries are keyed by an idempotency
// key, so posting the same business event twice is a no-op. The database
// enforces balance and append-only on its own, independent of this code.
package ledger

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"delos/internal/db"
	"delos/internal/money"
)

// Account is a ledger account code. The prefix sets the normal side.
type Account string

const (
	// Deposits is USDT sitting on our deposit addresses.
	Deposits Account = "asset:deposits"
	// Fees is service revenue from processing fees.
	Fees Account = "rev:fees"
	// RefundsPayable is USDT owed back to senders of rejected payments.
	RefundsPayable Account = "liab:refunds_payable"
)

// PartnerHold is confirmed money waiting for AML. Not yet the partner's.
func PartnerHold(partnerID string) Account {
	return Account("liab:partner:" + partnerID + ":hold")
}

// PartnerAvailable is money credited to the partner.
func PartnerAvailable(partnerID string) Account {
	return Account("liab:partner:" + partnerID + ":available")
}

func (a Account) normalSide() string {
	if strings.HasPrefix(string(a), "asset:") || strings.HasPrefix(string(a), "exp:") {
		return "debit"
	}
	return "credit"
}

// Posting is one line of an entry. Use Debit and Credit to build it.
type Posting struct {
	Account Account
	Amount  int64 // > 0 debit, < 0 credit
}

func Debit(a Account, amt money.Amount) Posting  { return Posting{a, int64(amt)} }
func Credit(a Account, amt money.Amount) Posting { return Posting{a, -int64(amt)} }

type Entry struct {
	IdempotencyKey string
	Kind           string
	Postings       []Posting
}

var ErrUnbalanced = errors.New("ledger entry does not balance")

// Post writes the entry inside tx. It returns false without writing anything
// if an entry with the same idempotency key already exists.
//
// Zero-amount postings are dropped (e.g. a 0% fee), so callers do not have to
// special-case them.
func Post(ctx context.Context, tx pgx.Tx, e Entry) (bool, error) {
	postings := make([]Posting, 0, len(e.Postings))
	var sum int64
	for _, p := range e.Postings {
		if p.Amount == 0 {
			continue
		}
		postings = append(postings, p)
		sum += p.Amount
	}
	if sum != 0 || len(postings) < 2 {
		return false, fmt.Errorf("%w: key=%s sum=%d postings=%d", ErrUnbalanced, e.IdempotencyKey, sum, len(postings))
	}

	var entryID int64
	err := tx.QueryRow(ctx, `
		INSERT INTO ledger_entries (idempotency_key, kind) VALUES ($1, $2)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id`, e.IdempotencyKey, e.Kind).Scan(&entryID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("error inserting entry: %w", err)
	}

	for _, p := range postings {
		accountID, err := ensureAccount(ctx, tx, p.Account)
		if err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO ledger_postings (entry_id, account_id, amount) VALUES ($1, $2, $3)`,
			entryID, accountID, p.Amount); err != nil {
			return false, fmt.Errorf("error inserting posting: %w", err)
		}
	}
	return true, nil
}

// ensureAccount looks the account up first and inserts it only if missing.
// An upsert with DO UPDATE would lock the account row until commit and
// serialise every concurrent posting to the same partner.
func ensureAccount(ctx context.Context, tx pgx.Tx, a Account) (int64, error) {
	const sel = `SELECT id FROM ledger_accounts WHERE code = $1`
	var id int64
	err := tx.QueryRow(ctx, sel, string(a)).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("error selecting account %s: %w", a, err)
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO ledger_accounts (code, normal_side) VALUES ($1, $2)
		ON CONFLICT (code) DO NOTHING
		RETURNING id`, string(a), a.normalSide()).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// Created by a concurrent transaction that has committed meanwhile.
		err = tx.QueryRow(ctx, sel, string(a)).Scan(&id)
	}
	if err != nil {
		return 0, fmt.Errorf("error ensuring account %s: %w", a, err)
	}
	return id, nil
}

// Balance returns the account balance on its normal side: a positive number
// means the account holds money (an asset we have, a liability we owe).
func Balance(ctx context.Context, q db.Querier, a Account) (money.Amount, error) {
	var raw int64
	err := q.QueryRow(ctx, `
		SELECT COALESCE(SUM(p.amount), 0)
		FROM ledger_postings p JOIN ledger_accounts a ON a.id = p.account_id
		WHERE a.code = $1`, string(a)).Scan(&raw)
	if err != nil {
		return 0, fmt.Errorf("error getting balance of %s: %w", a, err)
	}
	if a.normalSide() == "credit" {
		raw = -raw
	}
	return money.Amount(raw), nil
}

// Balances returns normal-side balances of all accounts.
func Balances(ctx context.Context, q db.Querier) (map[Account]money.Amount, error) {
	rows, err := q.Query(ctx, `
		SELECT a.code, a.normal_side, COALESCE(SUM(p.amount), 0)
		FROM ledger_accounts a LEFT JOIN ledger_postings p ON p.account_id = a.id
		GROUP BY a.code, a.normal_side`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[Account]money.Amount{}
	for rows.Next() {
		var code, side string
		var raw int64
		if err := rows.Scan(&code, &side, &raw); err != nil {
			return nil, err
		}
		if side == "credit" {
			raw = -raw
		}
		out[Account(code)] = money.Amount(raw)
	}
	return out, rows.Err()
}
