package app_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"delos/internal/aml"
	"delos/internal/chain"
	"delos/internal/ledger"
	"delos/internal/processor"
)

// hangingAML blocks until the caller's context is cancelled, like a provider
// that never answers while the worker process is being killed.
type hangingAML struct {
	called chan struct{}
}

func (a *hangingAML) Check(ctx context.Context, _ chain.Address) (aml.Result, error) {
	close(a.called)
	<-ctx.Done()
	return aml.Result{}, ctx.Err()
}

// A worker takes a transfer and dies before settling it. The transfer stays
// on hold, nobody else touches it while the lease is valid, and once the lease
// expires another worker settles it exactly once.
func TestCrashedWorker_TransferPickedUpAfterLeaseExpires(t *testing.T) {
	h := newHarness(t)
	inv := h.createInvoice(acmeKey, "order-crash", "100")
	h.send(customer, inv.DepositAddress, "100")
	h.mine(confirmations)
	h.watch()

	const lease = time.Minute
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := processor.Config{Lease: lease, AMLTimeout: time.Hour}

	// Worker A claims the transfer, hangs in the AML call and is killed: its
	// context is cancelled, so it can neither settle nor schedule a retry.
	hanging := &hangingAML{called: make(chan struct{})}
	workerA := processor.New(h.pool, hanging, cfg, logger)
	ctxA, kill := context.WithCancel(h.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = workerA.Tick(ctxA)
	}()
	<-hanging.called
	kill()
	<-done

	if n := h.count(`SELECT count(*) FROM transfers WHERE status = 'CONFIRMED' AND aml_next_at > now()`); n != 1 {
		t.Fatalf("transfer is not left leased and unsettled after the crash")
	}
	if b := h.balance(acmeKey); b.Available != "0.000000" || b.OnHold != "100.000000" {
		t.Fatalf("balance after crash = %+v, want everything on hold", b)
	}

	// Worker B runs while the lease is still valid: it must not take the transfer.
	now := time.Now()
	workerB := processor.New(h.pool, h.app.AML, cfg, logger)
	workerB.SetClock(func() time.Time { return now })
	if n, err := workerB.Tick(h.ctx); err != nil || n != 0 {
		t.Fatalf("worker B during lease: settled %d, err %v; want 0", n, err)
	}
	if calls := h.app.AML.Calls(); calls != 0 {
		t.Fatalf("aml called %d times during the lease, want 0", calls)
	}

	// The lease expires: worker B picks the transfer up and credits it.
	workerB.SetClock(func() time.Time { return now.Add(lease + time.Second) })
	if n, err := workerB.Tick(h.ctx); err != nil || n != 1 {
		t.Fatalf("worker B after lease: settled %d, err %v; want 1", n, err)
	}
	expectStatus(t, h.invoice(acmeKey, inv.ID), "PAID")

	// Further ticks by any worker change nothing.
	if n, err := workerB.Tick(h.ctx); err != nil || n != 0 {
		t.Fatalf("extra tick settled %d, err %v", n, err)
	}
	h.process()
	h.assertLedger(map[ledger.Account]string{
		ledger.Deposits:                 "100.000000",
		ledger.PartnerAvailable("acme"): "99.000000",
		ledger.Fees:                     "1.000000",
	})
	if n := h.count(`SELECT count(*) FROM ledger_entries WHERE kind = 'payment_credited'`); n != 1 {
		t.Fatalf("credit entries = %d, want 1", n)
	}
}
