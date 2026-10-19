// Package processor runs AML on confirmed transfers and settles them: credits
// the partner minus the fee, or creates a refund when the risk is high.
//
// The flow per transfer is claim → AML call → settle:
//   - claim takes a lease (aml_next_at in the future) under FOR UPDATE SKIP
//     LOCKED, so parallel workers pick different transfers;
//   - the AML call runs outside any transaction;
//   - settle locks the transfer and re-checks it is still CONFIRMED, so a
//     worker that lost its lease or a replay after a crash is a no-op.
//
// Ledger entries and refunds have unique keys per transfer, so even a bug in
// the above could not credit twice or create a second refund.
package processor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"delos/internal/aml"
	"delos/internal/chain"
	"delos/internal/invoice"
	"delos/internal/ledger"
	"delos/internal/money"
)

const (
	RefundNeedsAddress = "NEEDS_ADDRESS" // exchange wallet: ask the sender where to refund
	RefundReadyToSend  = "READY_TO_SEND"
)

type Config struct {
	BatchSize    int
	Lease        time.Duration // how long a claimed transfer is invisible to other workers
	AMLTimeout   time.Duration
	RetryBackoff time.Duration // first retry delay, doubled per attempt
	MaxBackoff   time.Duration
}

type Processor struct {
	pool *pgxpool.Pool
	aml  aml.Checker
	cfg  Config
	log  *slog.Logger
	now  func() time.Time
}

func New(pool *pgxpool.Pool, checker aml.Checker, cfg Config, log *slog.Logger) *Processor {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 50
	}
	if cfg.Lease <= 0 {
		cfg.Lease = time.Minute
	}
	if cfg.AMLTimeout <= 0 {
		cfg.AMLTimeout = 10 * time.Second
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 10 * time.Minute
	}
	return &Processor{pool: pool, aml: checker, cfg: cfg, log: log, now: time.Now}
}

// SetClock overrides the time source, for tests.
func (p *Processor) SetClock(now func() time.Time) { p.now = now }

func (p *Processor) Run(ctx context.Context, interval time.Duration) error {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := p.Tick(ctx); err != nil && ctx.Err() == nil {
			p.log.Error("processor tick failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Tick processes one batch of due transfers and returns how many were settled.
func (p *Processor) Tick(ctx context.Context) (int, error) {
	batch, err := p.claim(ctx)
	if err != nil {
		return 0, fmt.Errorf("error claiming transfers for aml: %w", err)
	}
	settled := 0
	var errs []error
	for _, t := range batch {
		ok, err := p.process(ctx, t)
		if err != nil {
			errs = append(errs, fmt.Errorf("error processing transfer %s: %w", t.id, err))
		}
		if ok {
			settled++
		}
	}
	return settled, errors.Join(errs...)
}

type claimed struct {
	id       uuid.UUID
	from     chain.Address
	attempts int
}

func (p *Processor) claim(ctx context.Context) ([]claimed, error) {
	now := p.now()
	rows, err := p.pool.Query(ctx, `
		UPDATE transfers SET aml_next_at = $1, updated_at = now()
		WHERE id IN (
			SELECT id FROM transfers
			WHERE status = $2 AND aml_next_at <= $3
			ORDER BY aml_next_at
			LIMIT $4
			FOR UPDATE SKIP LOCKED)
		RETURNING id, from_address, aml_attempts`,
		now.Add(p.cfg.Lease), invoice.TransferConfirmed, now, p.cfg.BatchSize)
	if err != nil {
		return nil, fmt.Errorf("error leasing due transfers: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (claimed, error) {
		var c claimed
		err := r.Scan(&c.id, &c.from, &c.attempts)
		return c, err
	})
}

func (p *Processor) process(ctx context.Context, t claimed) (bool, error) {
	actx, cancel := context.WithTimeout(ctx, p.cfg.AMLTimeout)
	res, err := p.aml.Check(actx, t.from)
	cancel()
	if err == nil && res.Risk != aml.RiskLow && res.Risk != aml.RiskHigh {
		err = fmt.Errorf("unknown risk level %q", res.Risk)
	}
	if err != nil {
		// Fail closed: the money stays on hold and we ask again later.
		return false, p.scheduleRetry(ctx, t, err)
	}
	return p.settle(ctx, t.id, res)
}

func (p *Processor) scheduleRetry(ctx context.Context, t claimed, cause error) error {
	delay := p.cfg.RetryBackoff << min(t.attempts, 20)
	if delay > p.cfg.MaxBackoff || delay < 0 {
		delay = p.cfg.MaxBackoff
	}
	_, err := p.pool.Exec(ctx, `
		UPDATE transfers SET aml_attempts = aml_attempts + 1, aml_next_at = $2, aml_last_error = $3, updated_at = now()
		WHERE id = $1 AND status = $4`,
		t.id, p.now().Add(delay), cause.Error(), invoice.TransferConfirmed)
	if err != nil {
		return fmt.Errorf("error scheduling aml retry: %w (aml error: %v)", err, cause)
	}
	p.log.Warn("aml check failed, will retry", "transfer", t.id, "attempt", t.attempts+1, "in", delay, "err", cause)
	return nil
}

func (p *Processor) settle(ctx context.Context, id uuid.UUID, res aml.Result) (bool, error) {
	settled := false
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		var (
			status, partnerID string
			invoiceID         uuid.UUID
			amount            int64
			feeBps            int
			from              chain.Address
		)
		err := tx.QueryRow(ctx, `
			SELECT t.status, t.partner_id, t.invoice_id, t.amount, t.from_address, i.fee_bps
			FROM transfers t JOIN invoices i ON i.id = t.invoice_id
			WHERE t.id = $1 FOR UPDATE OF t`, id).
			Scan(&status, &partnerID, &invoiceID, &amount, &from, &feeBps)
		if err != nil {
			return fmt.Errorf("error locking transfer: %w", err)
		}
		if status != invoice.TransferConfirmed {
			return nil // already settled by another worker or an earlier run
		}

		amt := money.Amount(amount)
		if res.Risk == aml.RiskLow {
			if err := credit(ctx, tx, id, partnerID, amt, feeBps); err != nil {
				return fmt.Errorf("error crediting partner %s: %w", partnerID, err)
			}
		} else {
			if err := refund(ctx, tx, id, invoiceID, partnerID, amt, from, res.Exchange); err != nil {
				return fmt.Errorf("error creating refund: %w", err)
			}
		}
		if err := invoice.RecomputeStatus(ctx, tx, invoiceID); err != nil {
			return fmt.Errorf("error recomputing invoice status: %w", err)
		}
		settled = true
		return nil
	})
	if err != nil {
		return false, err
	}
	if settled {
		p.log.Info("transfer settled", "transfer", id, "risk", res.Risk)
	}
	return settled, nil
}

// credit moves the amount from hold to the partner, keeping the fee.
func credit(ctx context.Context, tx pgx.Tx, id uuid.UUID, partnerID string, amt money.Amount, feeBps int) error {
	fee := amt.MulBps(feeBps)
	if err := postSettlement(ctx, tx, ledger.Entry{
		IdempotencyKey: settleKey(id),
		Kind:           "payment_credited",
		Postings: []ledger.Posting{
			ledger.Debit(ledger.PartnerHold(partnerID), amt),
			ledger.Credit(ledger.PartnerAvailable(partnerID), amt-fee),
			ledger.Credit(ledger.Fees, fee),
		},
	}); err != nil {
		return fmt.Errorf("error posting credit entry: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE transfers SET status = $2, aml_risk = $3, fee = $4, fee_bps = $5, aml_next_at = NULL, updated_at = now()
		WHERE id = $1`, id, invoice.TransferCredited, aml.RiskLow, int64(fee), feeBps); err != nil {
		return fmt.Errorf("error marking transfer credited: %w", err)
	}
	return nil
}

// refund moves the amount from hold to refunds payable and creates the refund.
// No fee is charged on a rejected payment. Gas is deducted when the refund is
// actually sent (see DESIGN.md §2.3), which is out of scope here.
func refund(ctx context.Context, tx pgx.Tx, id, invoiceID uuid.UUID, partnerID string, amt money.Amount,
	from chain.Address, exchange bool) error {
	status, to := RefundReadyToSend, &from
	if exchange {
		status, to = RefundNeedsAddress, nil
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO refunds (id, transfer_id, invoice_id, partner_id, amount, to_address, reason, status)
		VALUES ($1, $2, $3, $4, $5, $6, 'aml_high_risk', $7)
		ON CONFLICT (transfer_id) DO NOTHING`,
		uuid.New(), id, invoiceID, partnerID, int64(amt), to, status); err != nil {
		return fmt.Errorf("error inserting refund: %w", err)
	}
	if err := postSettlement(ctx, tx, ledger.Entry{
		IdempotencyKey: settleKey(id),
		Kind:           "payment_rejected",
		Postings: []ledger.Posting{
			ledger.Debit(ledger.PartnerHold(partnerID), amt),
			ledger.Credit(ledger.RefundsPayable, amt),
		},
	}); err != nil {
		return fmt.Errorf("error posting refund entry: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE transfers SET status = $2, aml_risk = $3, aml_next_at = NULL, updated_at = now()
		WHERE id = $1`, id, invoice.TransferRejected, aml.RiskHigh); err != nil {
		return fmt.Errorf("error marking transfer rejected: %w", err)
	}
	return nil
}

// settleKey is shared by credit and refund on purpose: the ledger accepts only
// one of them per transfer.
func settleKey(id uuid.UUID) string { return "transfer:" + id.String() + ":settle" }

// postSettlement posts the entry and treats an existing one as corruption: the
// caller has just seen the transfer as unsettled under a row lock.
func postSettlement(ctx context.Context, tx pgx.Tx, e ledger.Entry) error {
	ok, err := ledger.Post(ctx, tx, e)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("settlement entry %s exists for an unsettled transfer", e.IdempotencyKey)
	}
	return nil
}
