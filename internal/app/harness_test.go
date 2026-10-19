package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"delos/internal/app"
	"delos/internal/chain"
	"delos/internal/chain/sim"
	"delos/internal/config"
	"delos/internal/ledger"
	"delos/internal/money"
	"delos/internal/partner"
	"delos/internal/testutil"
)

func TestMain(m *testing.M) { testutil.Main(m) }

const (
	acmeKey   = "acme-secret-key-0001"   // fee 1%
	globexKey = "globex-secret-key-0002" // fee 2.5%

	confirmations = 3

	customer  = "0x1111111111111111111111111111111111111111"
	dirty     = "0xbad0000000000000000000000000000000000bad"
	exchange  = "0xe000000000000000000000000000000000000e00"
	usdt      = config.MainnetUSDT
	fakeToken = "0x2222222222222222222222222222222222222222"
)

type harness struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool
	cfg  config.Config
	app  *app.App
	srv  *httptest.Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, ctx: context.Background(), pool: testutil.NewDB(t)}
	h.cfg = config.Config{
		USDTContract:  usdt,
		Confirmations: confirmations,
		AddressSeed:   t.Name(),
		AMLBlacklist:  []string{dirty},
		AMLExchanges:  []string{exchange},
		Partners: []partner.Seed{
			{Partner: partner.Partner{ID: "acme", Name: "Acme", FeeBps: 100}, APIKey: acmeKey},
			{Partner: partner.Partner{ID: "globex", Name: "Globex", FeeBps: 250}, APIKey: globexKey},
		},
		SimEnabled: true,
	}
	h.restart()
	t.Cleanup(func() { h.srv.Close() })
	return h
}

// newApp builds another App on the same database, like a second replica.
func (h *harness) newApp() *app.App {
	h.t.Helper()
	a, err := app.New(h.ctx, h.cfg, h.pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		h.t.Fatalf("app.New: %v", err)
	}
	return a
}

// restart replaces the App with a fresh one, as a process restart would.
func (h *harness) restart() {
	h.t.Helper()
	a := h.newApp()
	h.app = a
	if h.srv != nil {
		h.srv.Close()
	}
	h.srv = httptest.NewServer(a.Handler)
}

type invoiceResp struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	Amount         string `json:"amount"`
	CreditedAmount string `json:"credited_amount"`
	DepositAddress string `json:"deposit_address"`
	Transfers      []struct {
		TxHash string  `json:"tx_hash"`
		Status string  `json:"status"`
		Amount string  `json:"amount"`
		Fee    *string `json:"fee"`
	} `json:"transfers"`
	Refunds []struct {
		ID        string  `json:"id"`
		Amount    string  `json:"amount"`
		ToAddress *string `json:"to_address"`
		Status    string  `json:"status"`
	} `json:"refunds"`
}

type balanceResp struct {
	Available string `json:"available"`
	OnHold    string `json:"on_hold"`
}

// do sends a request and decodes the JSON response into out (if non-nil).
func (h *harness) do(method, path, apiKey string, headers map[string]string, body any, out any) int {
	h.t.Helper()
	var r io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		r = bytes.NewBufferString(b)
	default:
		raw, _ := json.Marshal(b)
		r = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, r)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			h.t.Fatalf("decode %s %s: %v", method, path, err)
		}
	}
	return resp.StatusCode
}

func (h *harness) createInvoice(apiKey, idemKey, amount string) invoiceResp {
	h.t.Helper()
	var inv invoiceResp
	code := h.do("POST", "/v1/invoices", apiKey, map[string]string{"Idempotency-Key": idemKey},
		map[string]string{"amount": amount}, &inv)
	if code != http.StatusCreated && code != http.StatusOK {
		h.t.Fatalf("create invoice: status %d", code)
	}
	return inv
}

func (h *harness) invoice(apiKey, id string) invoiceResp {
	h.t.Helper()
	var inv invoiceResp
	if code := h.do("GET", "/v1/invoices/"+id, apiKey, nil, nil, &inv); code != http.StatusOK {
		h.t.Fatalf("get invoice: status %d", code)
	}
	return inv
}

func (h *harness) balance(apiKey string) balanceResp {
	h.t.Helper()
	var b balanceResp
	if code := h.do("GET", "/v1/balance", apiKey, nil, nil, &b); code != http.StatusOK {
		h.t.Fatalf("balance: status %d", code)
	}
	return b
}

func (h *harness) send(from, to, amount string) chain.TransferLog {
	h.t.Helper()
	return h.sendToken(usdt, from, to, amount, false)
}

func (h *harness) sendToken(token, from, to, amount string, failed bool) chain.TransferLog {
	h.t.Helper()
	a, err := money.Parse(amount)
	if err != nil {
		h.t.Fatal(err)
	}
	l, err := h.app.Sim.Transfer(h.ctx, sim.TransferRequest{
		Token: chain.Address(token), From: chain.Address(from), To: chain.Address(to),
		Amount: big.NewInt(int64(a)), Failed: failed,
	})
	if err != nil {
		h.t.Fatalf("sim transfer: %v", err)
	}
	return l
}

func (h *harness) mine(n int) {
	h.t.Helper()
	if _, err := h.app.Sim.Mine(h.ctx, n); err != nil {
		h.t.Fatalf("mine: %v", err)
	}
}

func (h *harness) watch() {
	h.t.Helper()
	if err := h.app.Watcher.Tick(h.ctx); err != nil {
		h.t.Fatalf("watcher tick: %v", err)
	}
}

func (h *harness) process() int {
	h.t.Helper()
	n, err := h.app.Processor.Tick(h.ctx)
	if err != nil {
		h.t.Fatalf("processor tick: %v", err)
	}
	return n
}

func (h *harness) tick() {
	h.t.Helper()
	h.watch()
	h.process()
}

func (h *harness) count(sql string, args ...any) int {
	h.t.Helper()
	var n int
	if err := h.pool.QueryRow(h.ctx, sql, args...).Scan(&n); err != nil {
		h.t.Fatalf("count: %v", err)
	}
	return n
}

// assertLedger checks the ledger invariant and the expected balances of the
// accounts listed; accounts not listed must be zero.
func (h *harness) assertLedger(want map[ledger.Account]string) {
	h.t.Helper()
	got, err := ledger.Balances(h.ctx, h.pool)
	if err != nil {
		h.t.Fatal(err)
	}
	// Assets equal liabilities plus revenue: every unit on our addresses is
	// accounted for as owed to someone or earned.
	var assets, claims money.Amount
	for acc, bal := range got {
		if acc[:6] == "asset:" {
			assets += bal
		} else {
			claims += bal
		}
	}
	if assets != claims {
		h.t.Errorf("ledger does not balance: assets %s, liabilities+revenue %s", assets, claims)
	}
	for acc, w := range want {
		if g := got[acc].String(); g != w {
			h.t.Errorf("balance %s = %s, want %s", acc, g, w)
		}
	}
	for acc, bal := range got {
		if _, ok := want[acc]; !ok && bal != 0 {
			h.t.Errorf("unexpected balance %s = %s", acc, bal)
		}
	}
}

func expectStatus(t *testing.T, inv invoiceResp, want string) {
	t.Helper()
	if inv.Status != want {
		t.Fatalf("invoice status = %s, want %s", inv.Status, want)
	}
}
