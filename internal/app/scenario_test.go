package app_test

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"delos/internal/app"
	"delos/internal/ledger"
	"delos/internal/watcher"
)

// The whole flow end to end: invoice → transfer → N confirmations → AML →
// credit minus fee. Checks what the partner sees at every step.
func TestFullScenario_CreditMinusFee(t *testing.T) {
	h := newHarness(t)
	inv := h.createInvoice(acmeKey, "order-1", "100")
	expectStatus(t, inv, "NEW")

	h.send(customer, inv.DepositAddress, "100")
	h.tick() // 1 confirmation
	expectStatus(t, h.invoice(acmeKey, inv.ID), "CONFIRMING")
	if b := h.balance(acmeKey); b.Available != "0.000000" || b.OnHold != "0.000000" {
		t.Fatalf("balance before confirmations = %+v", b)
	}

	h.mine(confirmations - 2)
	h.tick() // N-1 confirmations: still nothing
	expectStatus(t, h.invoice(acmeKey, inv.ID), "CONFIRMING")
	h.assertLedger(nil)

	h.mine(1)
	h.watch() // N confirmations, AML not run yet
	expectStatus(t, h.invoice(acmeKey, inv.ID), "AML_REVIEW")
	if b := h.balance(acmeKey); b.Available != "0.000000" || b.OnHold != "100.000000" {
		t.Fatalf("balance during AML = %+v, want 0 available / 100 on hold", b)
	}

	if n := h.process(); n != 1 {
		t.Fatalf("settled %d, want 1", n)
	}
	got := h.invoice(acmeKey, inv.ID)
	expectStatus(t, got, "PAID")
	if got.CreditedAmount != "100.000000" || len(got.Transfers) != 1 || *got.Transfers[0].Fee != "1.000000" {
		t.Fatalf("invoice after credit = %+v", got)
	}
	if b := h.balance(acmeKey); b.Available != "99.000000" || b.OnHold != "0.000000" {
		t.Fatalf("balance = %+v, want 99 available", b)
	}
	h.assertLedger(map[ledger.Account]string{
		ledger.Deposits:                 "100.000000",
		ledger.PartnerAvailable("acme"): "99.000000",
		ledger.Fees:                     "1.000000",
	})

	// Nothing else happens on further ticks and blocks.
	h.mine(5)
	h.tick()
	h.tick()
	if b := h.balance(acmeKey); b.Available != "99.000000" {
		t.Fatalf("balance changed after extra ticks: %+v", b)
	}
}

func TestFeeIsPerPartnerAndRoundsDown(t *testing.T) {
	h := newHarness(t)
	inv := h.createInvoice(globexKey, "g-1", "12.345678")
	h.send(customer, inv.DepositAddress, "12.345678")
	h.mine(confirmations)
	h.tick()

	// 2.5% of 12.345678 = 0.30864195 → fee 0.308641, partner gets the remainder.
	if b := h.balance(globexKey); b.Available != "12.037037" {
		t.Fatalf("globex available = %s, want 12.037037", b.Available)
	}
	h.assertLedger(map[ledger.Account]string{
		ledger.Deposits:                   "12.345678",
		ledger.PartnerAvailable("globex"): "12.037037",
		ledger.Fees:                       "0.308641",
	})
}

func TestHighRisk_CreatesExactlyOneRefund(t *testing.T) {
	h := newHarness(t)
	inv := h.createInvoice(acmeKey, "order-dirty", "50")
	h.send(dirty, inv.DepositAddress, "50")
	h.mine(confirmations)
	h.tick()

	got := h.invoice(acmeKey, inv.ID)
	expectStatus(t, got, "REFUNDING")
	if len(got.Refunds) != 1 {
		t.Fatalf("refunds = %d, want 1", len(got.Refunds))
	}
	r := got.Refunds[0]
	if r.Status != "READY_TO_SEND" || r.Amount != "50.000000" || r.ToAddress == nil || *r.ToAddress != dirty {
		t.Fatalf("refund = %+v", r)
	}
	if b := h.balance(acmeKey); b.Available != "0.000000" || b.OnHold != "0.000000" {
		t.Fatalf("partner balance after rejection = %+v", b)
	}
	want := map[ledger.Account]string{
		ledger.Deposits:       "50.000000",
		ledger.RefundsPayable: "50.000000",
	}
	h.assertLedger(want)

	// Re-running workers, restarting, and running in parallel must not create
	// a second refund or move money again.
	h.tick()
	h.restart()
	h.tick()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { _, _ = h.app.Processor.Tick(h.ctx) })
	}
	wg.Wait()

	if n := h.count(`SELECT count(*) FROM refunds`); n != 1 {
		t.Fatalf("refunds after replays = %d, want 1", n)
	}
	h.assertLedger(want)
}

func TestExchangeSender_RefundWaitsForAddress(t *testing.T) {
	h := newHarness(t)
	h.cfg.AMLBlacklist = append(h.cfg.AMLBlacklist, exchange)
	h.restart()

	inv := h.createInvoice(acmeKey, "order-ex", "10")
	h.send(exchange, inv.DepositAddress, "10")
	h.mine(confirmations)
	h.tick()

	got := h.invoice(acmeKey, inv.ID)
	if len(got.Refunds) != 1 || got.Refunds[0].Status != "NEEDS_ADDRESS" || got.Refunds[0].ToAddress != nil {
		t.Fatalf("refund to exchange = %+v, want NEEDS_ADDRESS without address", got.Refunds)
	}
}

func TestAMLUnavailable_MoneyWaitsAndRetries(t *testing.T) {
	h := newHarness(t)
	h.cfg.AMLRetryBackoff = 0 // retry on every tick
	h.restart()

	inv := h.createInvoice(acmeKey, "order-aml", "20")
	h.send(customer, inv.DepositAddress, "20")
	h.mine(confirmations)

	h.app.AML.SetAvailable(false)
	for range 3 {
		h.watch()
		if _, err := h.app.Processor.Tick(h.ctx); err != nil {
			t.Fatalf("tick with AML down: %v", err)
		}
	}
	expectStatus(t, h.invoice(acmeKey, inv.ID), "AML_REVIEW")
	if b := h.balance(acmeKey); b.Available != "0.000000" || b.OnHold != "20.000000" {
		t.Fatalf("balance while AML down = %+v", b)
	}
	if n := h.count(`SELECT aml_attempts FROM transfers`); n != 3 {
		t.Fatalf("aml_attempts = %d, want 3", n)
	}
	if calls := h.app.AML.Calls(); calls != 3 {
		t.Fatalf("aml calls = %d, want 3", calls)
	}

	h.app.AML.SetAvailable(true)
	h.process()
	expectStatus(t, h.invoice(acmeKey, inv.ID), "PAID")
	if b := h.balance(acmeKey); b.Available != "19.800000" {
		t.Fatalf("balance after AML recovered = %+v", b)
	}
}

func TestAMLRetryBackoff_NotRetriedEarly(t *testing.T) {
	h := newHarness(t)
	h.cfg.AMLRetryBackoff = time.Minute
	h.restart()
	inv := h.createInvoice(acmeKey, "order-backoff", "20")
	h.send(customer, inv.DepositAddress, "20")
	h.mine(confirmations)
	h.watch()

	h.app.AML.SetAvailable(false)
	h.process()
	h.app.AML.SetAvailable(true)
	h.process() // the transfer is not due for another minute
	if calls := h.app.AML.Calls(); calls != 1 {
		t.Fatalf("aml calls = %d, want 1 (retry must wait for backoff)", calls)
	}
	expectStatus(t, h.invoice(acmeKey, inv.ID), "AML_REVIEW")
}

func TestReorgBeforeConfirmations_LeavesNoCredit(t *testing.T) {
	h := newHarness(t)
	inv := h.createInvoice(acmeKey, "order-reorg", "30")
	h.send(customer, inv.DepositAddress, "30")
	h.mine(confirmations - 2)
	h.tick()
	expectStatus(t, h.invoice(acmeKey, inv.ID), "CONFIRMING")

	// The block with the transfer is replaced; the transfer is gone.
	if err := h.app.Sim.Reorg(h.ctx, confirmations-1, false); err != nil {
		t.Fatal(err)
	}
	h.tick()
	got := h.invoice(acmeKey, inv.ID)
	expectStatus(t, got, "NEW")
	if len(got.Transfers) != 1 || got.Transfers[0].Status != "ORPHANED" {
		t.Fatalf("transfers after reorg = %+v", got.Transfers)
	}

	h.mine(10)
	h.tick()
	expectStatus(t, h.invoice(acmeKey, inv.ID), "NEW")
	if b := h.balance(acmeKey); b.Available != "0.000000" || b.OnHold != "0.000000" {
		t.Fatalf("balance after reorg = %+v", b)
	}
	h.assertLedger(nil)
	if n := h.count(`SELECT count(*) FROM ledger_entries`); n != 0 {
		t.Fatalf("ledger entries = %d, want 0", n)
	}
}

// A reorged transaction that is mined again in another block is the same
// payment and must be credited exactly once.
func TestReorgReincludedTransfer_CreditedOnce(t *testing.T) {
	h := newHarness(t)
	inv := h.createInvoice(acmeKey, "order-reinc", "40")
	h.send(customer, inv.DepositAddress, "40")
	h.tick()

	if err := h.app.Sim.Reorg(h.ctx, 1, true); err != nil {
		t.Fatal(err)
	}
	h.tick()
	h.mine(confirmations)
	h.tick()

	got := h.invoice(acmeKey, inv.ID)
	expectStatus(t, got, "PAID")
	statuses := map[string]int{}
	for _, tr := range got.Transfers {
		statuses[tr.Status]++
	}
	if statuses["ORPHANED"] != 1 || statuses["CREDITED"] != 1 {
		t.Fatalf("transfer statuses = %v, want one ORPHANED and one CREDITED", statuses)
	}
	h.assertLedger(map[ledger.Account]string{
		ledger.Deposits:                 "40.000000",
		ledger.PartnerAvailable("acme"): "39.600000",
		ledger.Fees:                     "0.400000",
	})
}

// A reorg that reverts an already confirmed transfer cannot be fixed
// automatically: the watcher stops and the ledger is left untouched.
func TestDeepReorg_StopsWatcher(t *testing.T) {
	h := newHarness(t)
	inv := h.createInvoice(acmeKey, "order-deep", "5")
	h.send(customer, inv.DepositAddress, "5")
	h.mine(confirmations)
	h.tick()
	expectStatus(t, h.invoice(acmeKey, inv.ID), "PAID")

	if err := h.app.Sim.Reorg(h.ctx, confirmations+1, false); err != nil {
		t.Fatal(err)
	}
	if err := h.app.Watcher.Tick(h.ctx); !errors.Is(err, watcher.ErrDeepReorg) {
		t.Fatalf("watcher err = %v, want ErrDeepReorg", err)
	}
	if b := h.balance(acmeKey); b.Available != "4.950000" {
		t.Fatalf("balance changed by deep reorg: %+v", b)
	}
}

func TestIgnoresFakeTokenAndFailedTransaction(t *testing.T) {
	h := newHarness(t)
	inv := h.createInvoice(acmeKey, "order-fake", "100")
	h.sendToken(fakeToken, customer, inv.DepositAddress, "100", false) // "USDT" at another contract
	h.sendToken(usdt, customer, inv.DepositAddress, "100", true)       // reverted tx
	h.mine(confirmations)
	h.tick()

	got := h.invoice(acmeKey, inv.ID)
	expectStatus(t, got, "NEW")
	if len(got.Transfers) != 0 {
		t.Fatalf("transfers = %+v, want none", got.Transfers)
	}
	h.assertLedger(nil)
}

// Many transfers settled by parallel watchers and processors, then a restart:
// every transfer is recorded and credited exactly once.
func TestNoDoubleCredit_ParallelWorkersAndRestart(t *testing.T) {
	h := newHarness(t)
	const invoices = 20
	for i := range invoices {
		inv := h.createInvoice(acmeKey, fmt.Sprintf("order-%d", i), "10")
		h.send(customer, inv.DepositAddress, "10")
	}
	h.mine(confirmations)

	// Several app instances share the DB, like replicas would.
	instances := []*app.App{h.app}
	for range 3 {
		instances = append(instances, h.newApp())
	}
	var wg sync.WaitGroup
	for _, inst := range instances {
		for range 2 {
			wg.Go(func() {
				for range 3 {
					_ = inst.Watcher.Tick(h.ctx)
					_, _ = inst.Processor.Tick(h.ctx)
				}
			})
		}
	}
	wg.Wait()
	h.restart()
	h.tick()
	h.tick()

	if n := h.count(`SELECT count(*) FROM transfers`); n != invoices {
		t.Fatalf("transfers = %d, want %d", n, invoices)
	}
	if n := h.count(`SELECT count(*) FROM transfers WHERE status = 'CREDITED'`); n != invoices {
		t.Fatalf("credited = %d, want %d", n, invoices)
	}
	if n := h.count(`SELECT count(*) FROM ledger_entries`); n != 2*invoices {
		t.Fatalf("ledger entries = %d, want %d (confirm + settle per transfer)", n, 2*invoices)
	}
	h.assertLedger(map[ledger.Account]string{
		ledger.Deposits:                 "200.000000",
		ledger.PartnerAvailable("acme"): "198.000000",
		ledger.Fees:                     "2.000000",
	})
}

func TestInvoiceCreationIsIdempotent(t *testing.T) {
	h := newHarness(t)
	hdr := map[string]string{"Idempotency-Key": "k-1"}
	body := map[string]string{"amount": "15.5"}

	var first, second invoiceResp
	if code := h.do("POST", "/v1/invoices", acmeKey, hdr, body, &first); code != http.StatusCreated {
		t.Fatalf("first create = %d", code)
	}
	if code := h.do("POST", "/v1/invoices", acmeKey, hdr, body, &second); code != http.StatusOK {
		t.Fatalf("replay = %d, want 200", code)
	}
	if first.ID != second.ID || first.DepositAddress != second.DepositAddress {
		t.Fatalf("replay returned a different invoice: %s vs %s", first.ID, second.ID)
	}

	// Same key, different body: refuse instead of returning the old invoice.
	if code := h.do("POST", "/v1/invoices", acmeKey, hdr, map[string]string{"amount": "16"}, nil); code != http.StatusUnprocessableEntity {
		t.Fatalf("key reuse = %d, want 422", code)
	}

	// The same key from another partner is a different invoice.
	other := h.createInvoice(globexKey, "k-1", "15.5")
	if other.ID == first.ID {
		t.Fatal("idempotency keys leak across partners")
	}

	// Concurrent requests with one key create one invoice.
	ids := make(chan string, 20)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			var inv invoiceResp
			code := h.do("POST", "/v1/invoices", acmeKey, map[string]string{"Idempotency-Key": "k-race"}, body, &inv)
			if code == http.StatusCreated || code == http.StatusOK {
				ids <- inv.ID
			}
		})
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		seen[id] = true
	}
	if len(seen) != 1 {
		t.Fatalf("concurrent creates returned %d distinct invoices", len(seen))
	}
	if n := h.count(`SELECT count(*) FROM invoices WHERE idempotency_key = 'k-race'`); n != 1 {
		t.Fatalf("invoices with k-race = %d, want 1", n)
	}
}

func TestInvoiceValidation(t *testing.T) {
	h := newHarness(t)
	hdr := map[string]string{"Idempotency-Key": "v"}
	cases := []struct {
		name string
		body string
		hdr  map[string]string
		want int
	}{
		{"amount as JSON number", `{"amount": 100.5}`, hdr, http.StatusBadRequest},
		{"too many decimals", `{"amount": "1.0000001"}`, hdr, http.StatusBadRequest},
		{"negative", `{"amount": "-1"}`, hdr, http.StatusBadRequest},
		{"zero", `{"amount": "0"}`, hdr, http.StatusUnprocessableEntity},
		{"unknown field", `{"amount": "1", "currency": "BTC"}`, hdr, http.StatusBadRequest},
		{"no idempotency key", `{"amount": "1"}`, nil, http.StatusBadRequest},
	}
	for _, c := range cases {
		if got := h.do("POST", "/v1/invoices", acmeKey, c.hdr, c.body, nil); got != c.want {
			t.Errorf("%s: status %d, want %d", c.name, got, c.want)
		}
	}
}

func TestPartnerIsolation(t *testing.T) {
	h := newHarness(t)
	inv := h.createInvoice(acmeKey, "secret-order", "100")
	h.send(customer, inv.DepositAddress, "100")
	h.mine(confirmations)
	h.tick()

	if code := h.do("GET", "/v1/invoices/"+inv.ID, globexKey, nil, nil, nil); code != http.StatusNotFound {
		t.Fatalf("other partner's invoice = %d, want 404", code)
	}
	if b := h.balance(globexKey); b.Available != "0.000000" || b.OnHold != "0.000000" {
		t.Fatalf("globex sees acme's money: %+v", b)
	}
	if b := h.balance(acmeKey); b.Available != "99.000000" {
		t.Fatalf("acme balance = %+v", b)
	}
	if code := h.do("GET", "/v1/balance", "", nil, nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("no key = %d, want 401", code)
	}
	if code := h.do("GET", "/v1/balance", "wrong-key", nil, nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("wrong key = %d, want 401", code)
	}
}

func TestPartialPayments(t *testing.T) {
	h := newHarness(t)
	inv := h.createInvoice(acmeKey, "order-partial", "100")
	h.send(customer, inv.DepositAddress, "60")
	h.mine(confirmations)
	h.tick()
	expectStatus(t, h.invoice(acmeKey, inv.ID), "PARTIALLY_PAID")

	h.send(customer, inv.DepositAddress, "40")
	h.mine(confirmations)
	h.tick()
	got := h.invoice(acmeKey, inv.ID)
	expectStatus(t, got, "PAID")
	if got.CreditedAmount != "100.000000" {
		t.Fatalf("credited = %s", got.CreditedAmount)
	}
}
