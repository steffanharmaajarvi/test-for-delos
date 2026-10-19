// Package invoice creates invoices and derives their status from transfers.
package invoice

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"delos/internal/address"
	"delos/internal/chain"
	"delos/internal/db"
	"delos/internal/money"
	"delos/internal/partner"
)

type Status string

const (
	StatusNew           Status = "NEW"            // waiting for payment
	StatusConfirming    Status = "CONFIRMING"     // transfer seen, waiting for N confirmations
	StatusAMLReview     Status = "AML_REVIEW"     // confirmed, money on hold until AML answers
	StatusPartiallyPaid Status = "PARTIALLY_PAID" // credited less than the invoice amount
	StatusPaid          Status = "PAID"           // credited at least the invoice amount
	StatusRefunding     Status = "REFUNDING"      // AML rejected, refund created
)

// Transfer statuses. They live here because invoice status is derived from them.
const (
	TransferDetected  = "DETECTED"
	TransferOrphaned  = "ORPHANED"
	TransferConfirmed = "CONFIRMED"
	TransferCredited  = "CREDITED"
	TransferRejected  = "REJECTED"
)

type Invoice struct {
	ID             uuid.UUID     `json:"id"`
	PartnerID      string        `json:"-"`
	ExternalID     *string       `json:"external_id,omitempty"`
	Amount         money.Amount  `json:"amount"`
	CreditedAmount money.Amount  `json:"credited_amount"`
	FeeBps         int           `json:"fee_bps"`
	DepositAddress chain.Address `json:"deposit_address"`
	Status         Status        `json:"status"`
	CreatedAt      time.Time     `json:"created_at"`
	UpdatedAt      time.Time     `json:"updated_at"`
}

type CreateRequest struct {
	Amount     money.Amount `json:"amount"`
	ExternalID *string      `json:"external_id,omitempty"`
}

var (
	ErrNotFound            = errors.New("invoice not found")
	ErrInvalidAmount       = errors.New("amount must be positive")
	ErrIdempotencyMismatch = errors.New("idempotency key was already used with a different request")
)

type Service struct {
	pool      *pgxpool.Pool
	addresses address.Generator
}

func NewService(pool *pgxpool.Pool, addresses address.Generator) *Service {
	return &Service{pool: pool, addresses: addresses}
}

// Create makes a new invoice, or returns the one previously created with the
// same idempotency key (created == false). Reusing a key with a different body
// is an error rather than a silent replay of the old invoice.
func (s *Service) Create(ctx context.Context, p partner.Partner, idempotencyKey string, req CreateRequest) (inv Invoice, created bool, err error) {
	if req.Amount <= 0 {
		return Invoice{}, false, ErrInvalidAmount
	}
	reqHash, err := requestHash(req)
	if err != nil {
		return Invoice{}, false, err
	}

	// Fast path for retries: avoids burning an address index.
	if inv, err := s.byIdempotencyKey(ctx, p.ID, idempotencyKey, reqHash); !errors.Is(err, ErrNotFound) {
		return inv, false, err
	}

	var index uint64
	if err := s.pool.QueryRow(ctx, `SELECT nextval('deposit_address_index')`).Scan(&index); err != nil {
		return Invoice{}, false, fmt.Errorf("error allocating address index: %w", err)
	}
	id := uuid.New()
	addr := s.addresses.Address(index)

	tag, err := s.pool.Exec(ctx, `
		INSERT INTO invoices (id, partner_id, idempotency_key, request_hash, external_id, amount, fee_bps,
			deposit_address, address_index, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (partner_id, idempotency_key) DO NOTHING`,
		id, p.ID, idempotencyKey, reqHash, req.ExternalID, int64(req.Amount), p.FeeBps, addr, index, StatusNew)

	if err != nil {
		return Invoice{}, false, fmt.Errorf("error inserting invoice: %w", err)
	}
	if tag.RowsAffected() == 0 {
		inv, err := s.byIdempotencyKey(ctx, p.ID, idempotencyKey, reqHash)
		return inv, false, err
	}
	inv, err = s.Get(ctx, p.ID, id)
	return inv, true, err
}

func (s *Service) byIdempotencyKey(ctx context.Context, partnerID, key string, reqHash []byte) (Invoice, error) {
	var storedHash []byte
	inv, err := scanInvoice(s.pool.QueryRow(ctx, `SELECT `+invoiceColumns+`, request_hash
		FROM invoices WHERE partner_id = $1 AND idempotency_key = $2`, partnerID, key), &storedHash)
	if err != nil {
		return Invoice{}, err
	}
	if string(storedHash) != string(reqHash) {
		return Invoice{}, ErrIdempotencyMismatch
	}
	return inv, nil
}

// Get returns the partner's invoice. Another partner's invoice is reported as
// not found, so its existence does not leak.
func (s *Service) Get(ctx context.Context, partnerID string, id uuid.UUID) (Invoice, error) {
	return scanInvoice(s.pool.QueryRow(ctx, `SELECT `+invoiceColumns+`
		FROM invoices WHERE id = $1 AND partner_id = $2`, id, partnerID))
}

const invoiceColumns = `id, partner_id, external_id, amount, credited_amount, fee_bps, deposit_address, status, created_at, updated_at`

func scanInvoice(row pgx.Row, extra ...any) (Invoice, error) {
	var inv Invoice
	var amount, credited int64
	dest := append([]any{&inv.ID, &inv.PartnerID, &inv.ExternalID, &amount, &credited, &inv.FeeBps,
		&inv.DepositAddress, &inv.Status, &inv.CreatedAt, &inv.UpdatedAt}, extra...)
	if err := row.Scan(dest...); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Invoice{}, ErrNotFound
		}
		return Invoice{}, err
	}
	inv.Amount, inv.CreditedAmount = money.Amount(amount), money.Amount(credited)
	return inv, nil
}

func requestHash(req CreateRequest) ([]byte, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	h := sha256.Sum256(b)
	return h[:], nil
}

// RecomputeStatus derives the invoice status from its transfers and stores it.
// It locks the invoice row first and reads transfers afterwards, so of two
// concurrent writers the one that commits last always sees both changes.
//
// Deriving status instead of moving it step by step means a reorg needs no
// special backward transition: orphaned transfers drop out of the aggregate.
func RecomputeStatus(ctx context.Context, tx pgx.Tx, invoiceID uuid.UUID) error {
	var amount int64
	if err := tx.QueryRow(ctx, `SELECT amount FROM invoices WHERE id = $1 FOR UPDATE`, invoiceID).Scan(&amount); err != nil {
		return fmt.Errorf("error locking invoice %s: %w", invoiceID, err)
	}

	var credited int64
	var confirmed, detected, rejected int
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount) FILTER (WHERE status = $2), 0),
		       COUNT(*) FILTER (WHERE status = $3),
		       COUNT(*) FILTER (WHERE status = $4),
		       COUNT(*) FILTER (WHERE status = $5)
		FROM transfers WHERE invoice_id = $1`,
		invoiceID, TransferCredited, TransferConfirmed, TransferDetected, TransferRejected).
		Scan(&credited, &confirmed, &detected, &rejected)
	if err != nil {
		return fmt.Errorf("error aggregating transfers: %w", err)
	}

	var st Status
	switch {
	case credited >= amount:
		st = StatusPaid
	case confirmed > 0:
		st = StatusAMLReview
	case detected > 0:
		st = StatusConfirming
	case credited > 0:
		st = StatusPartiallyPaid
	case rejected > 0:
		st = StatusRefunding
	default:
		st = StatusNew
	}
	_, err = tx.Exec(ctx, `
		UPDATE invoices SET status = $2, credited_amount = $3, updated_at = now()
		WHERE id = $1 AND (status <> $2 OR credited_amount <> $3)`, invoiceID, st, credited)
	return err
}

// RecomputeStatuses recomputes several invoices in a fixed order, so that
// transactions touching overlapping sets cannot deadlock.
func RecomputeStatuses(ctx context.Context, tx pgx.Tx, ids []uuid.UUID) error {
	ids = slices.Clone(ids)
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	ids = slices.Compact(ids)
	for _, id := range ids {
		if err := RecomputeStatus(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}

// Details is the invoice with its transfers and refunds, for the status API.
type Details struct {
	Invoice
	Transfers []TransferView `json:"transfers"`
	Refunds   []RefundView   `json:"refunds"`
}

type TransferView struct {
	TxHash      chain.Hash    `json:"tx_hash"`
	LogIndex    int           `json:"log_index"`
	BlockNumber uint64        `json:"block_number"`
	From        chain.Address `json:"from"`
	Amount      money.Amount  `json:"amount"`
	Status      string        `json:"status"`
	Fee         *money.Amount `json:"fee,omitempty"`
}

type RefundView struct {
	ID        uuid.UUID      `json:"id"`
	Amount    money.Amount   `json:"amount"`
	ToAddress *chain.Address `json:"to_address"`
	Reason    string         `json:"reason"`
	Status    string         `json:"status"`
}

func (s *Service) Details(ctx context.Context, partnerID string, id uuid.UUID) (Details, error) {
	inv, err := s.Get(ctx, partnerID, id)
	if err != nil {
		return Details{}, err
	}
	d := Details{Invoice: inv, Transfers: []TransferView{}, Refunds: []RefundView{}}
	d.Transfers, err = listTransfers(ctx, s.pool, id)
	if err != nil {
		return Details{}, err
	}
	d.Refunds, err = listRefunds(ctx, s.pool, id)
	return d, err
}

func listTransfers(ctx context.Context, q db.Querier, invoiceID uuid.UUID) ([]TransferView, error) {
	rows, err := q.Query(ctx, `
		SELECT tx_hash, log_index, block_number, from_address, amount, status, fee
		FROM transfers WHERE invoice_id = $1 ORDER BY block_number, log_index, created_at`, invoiceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TransferView{}
	for rows.Next() {
		var t TransferView
		var amount int64
		var fee *int64
		if err := rows.Scan(&t.TxHash, &t.LogIndex, &t.BlockNumber, &t.From, &amount, &t.Status, &fee); err != nil {
			return nil, err
		}
		t.Amount = money.Amount(amount)
		if fee != nil {
			f := money.Amount(*fee)
			t.Fee = &f
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func listRefunds(ctx context.Context, q db.Querier, invoiceID uuid.UUID) ([]RefundView, error) {
	rows, err := q.Query(ctx, `
		SELECT id, amount, to_address, reason, status FROM refunds WHERE invoice_id = $1 ORDER BY created_at`, invoiceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RefundView{}
	for rows.Next() {
		var r RefundView
		var amount int64
		if err := rows.Scan(&r.ID, &amount, &r.ToAddress, &r.Reason, &r.Status); err != nil {
			return nil, err
		}
		r.Amount = money.Amount(amount)
		out = append(out, r)
	}
	return out, rows.Err()
}
