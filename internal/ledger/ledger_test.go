package ledger_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"delos/internal/ledger"
	"delos/internal/money"
	"delos/internal/testutil"
)

func TestMain(m *testing.M) { testutil.Main(m) }

func TestPostIsIdempotent(t *testing.T) {
	ctx := context.Background()
	pool := testutil.NewDB(t)
	e := ledger.Entry{
		IdempotencyKey: "test:1",
		Kind:           "test",
		Postings: []ledger.Posting{
			ledger.Debit(ledger.Deposits, 100),
			ledger.Credit(ledger.PartnerHold("p"), 100),
		},
	}
	for i, want := range []bool{true, false} {
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			ok, err := ledger.Post(ctx, tx, e)
			if ok != want {
				t.Errorf("post #%d inserted = %v, want %v", i, ok, want)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	bal, err := ledger.Balance(ctx, pool, ledger.Deposits)
	if err != nil || bal != 100 {
		t.Fatalf("deposits = %d, %v; want 100", bal, err)
	}
	bal, _ = ledger.Balance(ctx, pool, ledger.PartnerHold("p"))
	if bal != 100 {
		t.Fatalf("hold = %d, want 100 (normal-side balance)", bal)
	}
}

func TestPostRejectsUnbalanced(t *testing.T) {
	ctx := context.Background()
	pool := testutil.NewDB(t)
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		_, err := ledger.Post(ctx, tx, ledger.Entry{
			IdempotencyKey: "bad", Kind: "test",
			Postings: []ledger.Posting{
				ledger.Debit(ledger.Deposits, 100),
				ledger.Credit(ledger.Fees, money.Amount(99)),
			},
		})
		return err
	})
	if !errors.Is(err, ledger.ErrUnbalanced) {
		t.Fatalf("err = %v, want ErrUnbalanced", err)
	}
}

// The database must reject an unbalanced entry even if the Go check is bypassed.
func TestDatabaseEnforcesBalanceAndAppendOnly(t *testing.T) {
	ctx := context.Background()
	pool := testutil.NewDB(t)

	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO ledger_accounts (code, normal_side) VALUES ('asset:x', 'debit');
			INSERT INTO ledger_entries (idempotency_key, kind) VALUES ('raw', 'test');
			INSERT INTO ledger_postings (entry_id, account_id, amount)
				SELECT e.id, a.id, 5 FROM ledger_entries e, ledger_accounts a;`)
		return err
	})
	if err == nil {
		t.Fatal("unbalanced entry committed")
	}

	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		_, err := ledger.Post(ctx, tx, ledger.Entry{
			IdempotencyKey: "ok", Kind: "test",
			Postings: []ledger.Posting{ledger.Debit(ledger.Deposits, 1), ledger.Credit(ledger.Fees, 1)},
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE ledger_postings SET amount = amount * 2`); err == nil {
		t.Fatal("posting update allowed")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM ledger_entries`); err == nil {
		t.Fatal("entry delete allowed")
	}
}
