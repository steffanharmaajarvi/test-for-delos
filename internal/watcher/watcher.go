// Package watcher scans the chain for USDT transfers to our deposit addresses,
// tracks confirmations and undoes transfers that a reorg removed.
//
// Terms used below:
//   - head: the latest block the node knows about;
//   - tip:  the latest block we have scanned and stored (chain_cursor).
//
// Each Tick:
//  1. checks that our tip is still on the canonical chain, rolling back to
//     the common ancestor if it is not;
//  2. scans blocks tip+1..head one by one, each in its own transaction,
//     verifying that every block links to the previous one by parent hash;
//  3. confirms transfers that have reached N confirmations counted from our
//     tip and moves their amount to the partner's hold account.
//
// Chain calls happen outside DB transactions; every write transaction locks
// the cursor row and re-checks the tip, so concurrent watchers (or a restart
// in the middle of a tick) cannot record the same block twice.
package watcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"delos/internal/chain"
	"delos/internal/db"
	"delos/internal/invoice"
	"delos/internal/ledger"
	"delos/internal/money"
)

type Config struct {
	Token         chain.Address // the one true USDT contract
	Confirmations uint64        // N; the block containing the tx counts as 1
	StartBlock    uint64        // where to start on an empty database
	MaxReorgDepth uint64        // deeper reorgs stop the watcher for a human
	BatchSize     int           // max blocks scanned per tick
}

// ErrDeepReorg means a reorg is deeper than we tolerate or reverts an already
// confirmed transfer. The watcher refuses to continue until an operator steps in.
var ErrDeepReorg = errors.New("reorg too deep: manual intervention required")

// errTipMoved means another watcher advanced the tip first.
var errTipMoved = errors.New("tip moved concurrently")

type Watcher struct {
	pool   *pgxpool.Pool
	client chain.Client
	cfg    Config
	log    *slog.Logger
	now    func() time.Time
}

func New(pool *pgxpool.Pool, client chain.Client, cfg Config, log *slog.Logger) *Watcher {
	if cfg.Confirmations == 0 {
		cfg.Confirmations = 1
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.MaxReorgDepth == 0 {
		cfg.MaxReorgDepth = 64
	}
	return &Watcher{pool: pool, client: client, cfg: cfg, log: log, now: time.Now}
}

// Run calls Tick every interval until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := w.Tick(ctx); err != nil && ctx.Err() == nil {
			w.log.Error("watcher tick failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (w *Watcher) Tick(ctx context.Context) error {
	if err := w.ensureCursor(ctx); err != nil {
		return fmt.Errorf("error initialising cursor: %w", err)
	}
	if err := w.verifyTip(ctx); err != nil {
		return fmt.Errorf("error verifying tip: %w", err)
	}
	if err := w.scanNewBlocks(ctx); err != nil {
		return fmt.Errorf("error scanning blocks: %w", err)
	}
	if err := w.confirmTransfers(ctx); err != nil {
		return fmt.Errorf("error confirming transfers: %w", err)
	}
	return nil
}

// ensureCursor stores the start block as the tip on the very first run.
func (w *Watcher) ensureCursor(ctx context.Context) error {
	var exists bool
	if err := w.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM chain_cursor)`).Scan(&exists); err != nil {
		return fmt.Errorf("error checking cursor: %w", err)
	}
	if exists {
		return nil
	}

	startBlock, err := w.client.BlockByNumber(ctx, w.cfg.StartBlock)
	if err != nil {
		return fmt.Errorf("error getting start block %d: %w", w.cfg.StartBlock, err)
	}

	return pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		result, err := tx.Exec(ctx, `INSERT INTO chain_cursor (id, block_number) VALUES (1, $1) ON CONFLICT DO NOTHING`,
			startBlock.Number)
		if err != nil {
			return fmt.Errorf("error inserting cursor: %w", err)
		}
		if result.RowsAffected() == 0 {
			return nil // another watcher initialised it first
		}
		return insertScannedBlock(ctx, tx, startBlock)
	})
}

// verifyTip detects a reorg that replaced our tip even when no newer block
// exists yet.
func (w *Watcher) verifyTip(ctx context.Context) error {
	tip, err := w.loadTip(ctx, w.pool)
	if err != nil {
		return err
	}
	canonical, err := w.client.BlockByNumber(ctx, tip.Number)
	if errors.Is(err, chain.ErrBlockNotFound) {
		// The chain got shorter than our tip: a reorg in progress.
		return w.rollbackReorg(ctx)
	}
	if err != nil {
		return fmt.Errorf("error getting block %d: %w", tip.Number, err)
	}
	if canonical.Hash != tip.Hash {
		return w.rollbackReorg(ctx)
	}
	return nil
}

func (w *Watcher) scanNewBlocks(ctx context.Context) error {
	head, err := w.client.HeadNumber(ctx)
	if err != nil {
		return fmt.Errorf("error getting chain head: %w", err)
	}
	for range w.cfg.BatchSize {
		tip, err := w.loadTip(ctx, w.pool)
		if err != nil {
			return err
		}
		if tip.Number >= head {
			return nil
		}

		next, err := w.client.BlockByNumber(ctx, tip.Number+1)
		if err != nil {
			return fmt.Errorf("error getting block %d: %w", tip.Number+1, err)
		}

		if next.ParentHash != tip.Hash {
			return w.rollbackReorg(ctx)
		}

		transfers, err := w.fetchIncomingTransfers(ctx, next)
		if err != nil {
			return fmt.Errorf("error fetching transfers of block %d: %w", next.Number, err)
		}
		err = w.recordBlock(ctx, next, tip, transfers)
		if errors.Is(err, errTipMoved) {
			continue
		}
		if err != nil {
			return fmt.Errorf("error recording block %d: %w", next.Number, err)
		}
	}
	return nil
}

// incomingTransfer is a USDT transfer to one of our deposit addresses.
type incomingTransfer struct {
	chain.TransferLog
	amount money.Amount
}

// fetchIncomingTransfers returns the block's genuine USDT transfers to our
// deposit addresses from successful transactions.
func (w *Watcher) fetchIncomingTransfers(ctx context.Context, block chain.Block) ([]incomingTransfer, error) {
	logs, err := w.client.TransferLogs(ctx, block.Hash, w.cfg.Token)
	if err != nil {
		return nil, fmt.Errorf("error getting transfer logs: %w", err)
	}

	var usdtLogs []chain.TransferLog
	var recipients []string
	for _, log := range logs {
		if chain.NormalizeAddress(string(log.Token)) != w.cfg.Token || log.BlockHash != block.Hash {
			continue
		}

		log.To, log.From = chain.NormalizeAddress(string(log.To)), chain.NormalizeAddress(string(log.From))
		usdtLogs = append(usdtLogs, log)
		recipients = append(recipients, string(log.To))
	}
	if len(usdtLogs) == 0 {
		return nil, nil
	}

	depositAddresses, err := w.filterDepositAddresses(ctx, recipients)
	if err != nil {
		return nil, err
	}

	var incoming []incomingTransfer
	for _, log := range usdtLogs {
		if !depositAddresses[log.To] {
			continue
		}
		succeeded, err := w.client.TxSucceeded(ctx, log.TxHash)
		if err != nil {
			return nil, fmt.Errorf("error getting receipt of tx %s: %w", log.TxHash, err)
		}
		if !succeeded {
			w.log.Warn("ignoring transfer from failed transaction", "tx", log.TxHash)
			continue
		}
		amount, err := money.FromBaseUnits(log.Amount)
		if err != nil {
			// Not possible with real USDT supply; refuse to skip silently.
			return nil, fmt.Errorf("error converting amount %s of tx %s: %w", log.Amount, log.TxHash, err)
		}
		if amount == 0 {
			continue
		}
		incoming = append(incoming, incomingTransfer{TransferLog: log, amount: amount})
	}
	return incoming, nil
}

// filterDepositAddresses returns which of the addresses belong to our invoices.
func (w *Watcher) filterDepositAddresses(ctx context.Context, addresses []string) (map[chain.Address]bool, error) {
	rows, err := w.pool.Query(ctx, `SELECT deposit_address FROM invoices WHERE deposit_address = ANY($1)`, addresses)
	if err != nil {
		return nil, fmt.Errorf("error querying deposit addresses: %w", err)
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[chain.Address])
	if err != nil {
		return nil, fmt.Errorf("error reading deposit addresses: %w", err)
	}
	set := make(map[chain.Address]bool, len(found))
	for _, address := range found {
		set[address] = true
	}
	return set, nil
}

// recordBlock stores the block's transfers and advances the tip to it, in one
// transaction. It fails with errTipMoved if the tip is no longer the block's
// parent, i.e. another watcher got there first.
func (w *Watcher) recordBlock(ctx context.Context, block chain.Block, parent scannedBlock, transfers []incomingTransfer) error {
	return pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		tip, err := w.lockTip(ctx, tx)
		if err != nil {
			return err
		}
		if tip != parent {
			return errTipMoved
		}

		var affectedInvoices []uuid.UUID
		for _, transfer := range transfers {
			invoiceID, inserted, err := w.insertTransfer(ctx, tx, block, transfer)
			if err != nil {
				return err
			}
			if inserted {
				affectedInvoices = append(affectedInvoices, invoiceID)
			}
		}
		if err := invoice.RecomputeStatuses(ctx, tx, affectedInvoices); err != nil {
			return fmt.Errorf("error recomputing invoice statuses: %w", err)
		}
		if err := insertScannedBlock(ctx, tx, block); err != nil {
			return err
		}
		return setTip(ctx, tx, block.Number)
	})
}

// insertTransfer records a detected transfer. inserted is false if the
// address has no invoice or the transfer is already recorded.
func (w *Watcher) insertTransfer(ctx context.Context, tx pgx.Tx, block chain.Block, transfer incomingTransfer) (invoiceID uuid.UUID, inserted bool, err error) {
	var partnerID string
	err = tx.QueryRow(ctx, `SELECT id, partner_id FROM invoices WHERE deposit_address = $1`, transfer.To).
		Scan(&invoiceID, &partnerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("error finding invoice by address %s: %w", transfer.To, err)
	}

	result, err := tx.Exec(ctx, `
		INSERT INTO transfers (id, invoice_id, partner_id, tx_hash, log_index, block_number, block_hash,
			from_address, to_address, amount, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT DO NOTHING`,
		uuid.New(), invoiceID, partnerID, transfer.TxHash, transfer.LogIndex, block.Number, block.Hash,
		transfer.From, transfer.To, int64(transfer.amount), invoice.TransferDetected)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("error inserting transfer %s: %w", transfer.TxHash, err)
	}
	if result.RowsAffected() == 0 {
		return invoiceID, false, nil
	}
	w.log.Info("transfer detected", "tx", transfer.TxHash, "invoice", invoiceID, "amount", transfer.amount, "block", block.Number)
	return invoiceID, true, nil
}

// confirmedTransfer is a transfer that has just reached N confirmations.
type confirmedTransfer struct {
	ID        uuid.UUID
	InvoiceID uuid.UUID
	PartnerID string
	Amount    money.Amount
	TxHash    chain.Hash
}

// confirmTransfers moves transfers with N confirmations from DETECTED to
// CONFIRMED and books them into the partner's hold account. Confirmations are
// counted from our tip, not the node's head: blocks we have not yet linked by
// parent hash do not count.
func (w *Watcher) confirmTransfers(ctx context.Context) error {
	return pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		tip, err := w.lockTip(ctx, tx)
		if err != nil {
			return err
		}
		if tip.Number+1 < w.cfg.Confirmations {
			return nil
		}
		// A transfer in this block has exactly N confirmations at our tip.
		lastConfirmedBlock := tip.Number + 1 - w.cfg.Confirmations

		rows, err := tx.Query(ctx, `
			UPDATE transfers SET status = $1, aml_next_at = $2, updated_at = now()
			WHERE status = $3 AND block_number <= $4
			RETURNING id, invoice_id, partner_id, amount, tx_hash`,
			invoice.TransferConfirmed, w.now(), invoice.TransferDetected, lastConfirmedBlock)
		if err != nil {
			return fmt.Errorf("error confirming transfers up to block %d: %w", lastConfirmedBlock, err)
		}
		confirmed, err := pgx.CollectRows(rows, pgx.RowToStructByPos[confirmedTransfer])
		if err != nil {
			return fmt.Errorf("error reading confirmed transfers: %w", err)
		}

		affectedInvoices := make([]uuid.UUID, 0, len(confirmed))
		for _, transfer := range confirmed {
			if err := holdConfirmedTransfer(ctx, tx, transfer); err != nil {
				return err
			}
			affectedInvoices = append(affectedInvoices, transfer.InvoiceID)
			w.log.Info("transfer confirmed, waiting for AML",
				"tx", transfer.TxHash, "invoice", transfer.InvoiceID, "amount", transfer.Amount)
		}
		if err := invoice.RecomputeStatuses(ctx, tx, affectedInvoices); err != nil {
			return fmt.Errorf("error recomputing invoice statuses: %w", err)
		}
		return nil
	})
}

// holdConfirmedTransfer books the confirmed amount into the partner's hold
// account, where it waits for AML.
func holdConfirmedTransfer(ctx context.Context, tx pgx.Tx, transfer confirmedTransfer) error {
	_, err := ledger.Post(ctx, tx, ledger.Entry{
		IdempotencyKey: "transfer:" + transfer.ID.String() + ":confirm",
		Kind:           "deposit_confirmed",
		Postings: []ledger.Posting{
			ledger.Debit(ledger.Deposits, transfer.Amount),
			ledger.Credit(ledger.PartnerHold(transfer.PartnerID), transfer.Amount),
		},
	})
	if err != nil {
		return fmt.Errorf("error posting hold for transfer %s: %w", transfer.ID, err)
	}
	return nil
}

// rollbackReorg moves the tip back to the last block that is still on the
// canonical chain and orphans transfers recorded in the dropped blocks.
func (w *Watcher) rollbackReorg(ctx context.Context) error {
	ancestor, err := w.findCommonAncestor(ctx)
	if err != nil {
		return fmt.Errorf("error finding common ancestor: %w", err)
	}

	return pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		tip, err := w.lockTip(ctx, tx)
		if err != nil {
			return err
		}
		if tip.Number <= ancestor {
			return nil // already rolled back by another watcher
		}

		// A confirmed (or later) transfer in a dropped block means the reorg
		// was deeper than N. Its money may already be with the partner, so we
		// stop rather than guess.
		var settledCount int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM transfers WHERE block_number > $1 AND status NOT IN ($2, $3)`,
			ancestor, invoice.TransferDetected, invoice.TransferOrphaned).Scan(&settledCount); err != nil {
			return fmt.Errorf("error counting settled transfers above block %d: %w", ancestor, err)
		}
		if settledCount > 0 {
			return fmt.Errorf("%w: %d confirmed transfers above block %d", ErrDeepReorg, settledCount, ancestor)
		}

		affectedInvoices, err := w.orphanTransfersAbove(ctx, tx, ancestor)
		if err != nil {
			return err
		}
		if err := invoice.RecomputeStatuses(ctx, tx, affectedInvoices); err != nil {
			return fmt.Errorf("error recomputing invoice statuses: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM scanned_blocks WHERE number > $1`, ancestor); err != nil {
			return fmt.Errorf("error deleting scanned blocks above %d: %w", ancestor, err)
		}
		if err := setTip(ctx, tx, ancestor); err != nil {
			return err
		}
		w.log.Warn("reorg rolled back", "from", tip.Number, "to", ancestor)
		return nil
	})
}

// orphanTransfersAbove marks detected transfers in blocks above the given
// number as ORPHANED and returns their invoices.
func (w *Watcher) orphanTransfersAbove(ctx context.Context, tx pgx.Tx, blockNumber uint64) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, `
		UPDATE transfers SET status = $1, updated_at = now()
		WHERE block_number > $2 AND status = $3
		RETURNING invoice_id, tx_hash`, invoice.TransferOrphaned, blockNumber, invoice.TransferDetected)
	if err != nil {
		return nil, fmt.Errorf("error orphaning transfers above block %d: %w", blockNumber, err)
	}
	type orphanedTransfer struct {
		InvoiceID uuid.UUID
		TxHash    chain.Hash
	}
	orphaned, err := pgx.CollectRows(rows, pgx.RowToStructByPos[orphanedTransfer])
	if err != nil {
		return nil, fmt.Errorf("error reading orphaned transfers: %w", err)
	}

	affectedInvoices := make([]uuid.UUID, 0, len(orphaned))
	for _, transfer := range orphaned {
		affectedInvoices = append(affectedInvoices, transfer.InvoiceID)
		w.log.Warn("transfer orphaned by reorg", "tx", transfer.TxHash, "invoice", transfer.InvoiceID)
	}
	return affectedInvoices, nil
}

// findCommonAncestor walks our scanned blocks from the tip down and returns
// the number of the first one whose hash still matches the canonical chain.
func (w *Watcher) findCommonAncestor(ctx context.Context) (uint64, error) {
	rows, err := w.pool.Query(ctx, `SELECT number, hash FROM scanned_blocks ORDER BY number DESC LIMIT $1`,
		w.cfg.MaxReorgDepth+1)
	if err != nil {
		return 0, fmt.Errorf("error querying scanned blocks: %w", err)
	}
	scanned, err := pgx.CollectRows(rows, pgx.RowToStructByPos[scannedBlock])
	if err != nil {
		return 0, fmt.Errorf("error reading scanned blocks: %w", err)
	}

	for _, stored := range scanned {
		canonical, err := w.client.BlockByNumber(ctx, stored.Number)
		if errors.Is(err, chain.ErrBlockNotFound) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("error getting block %d: %w", stored.Number, err)
		}
		if canonical.Hash == stored.Hash {
			return stored.Number, nil
		}
	}
	return 0, fmt.Errorf("%w: no common ancestor within %d blocks", ErrDeepReorg, w.cfg.MaxReorgDepth)
}

// scannedBlock is a block we have stored: our tip or one below it.
type scannedBlock struct {
	Number uint64
	Hash   chain.Hash
}

const selectTip = `
	SELECT c.block_number, b.hash
	FROM chain_cursor c JOIN scanned_blocks b ON b.number = c.block_number
	WHERE c.id = 1`

// loadTip reads the tip without locking.
func (w *Watcher) loadTip(ctx context.Context, q db.Querier) (scannedBlock, error) {
	var tip scannedBlock
	if err := q.QueryRow(ctx, selectTip).Scan(&tip.Number, &tip.Hash); err != nil {
		return scannedBlock{}, fmt.Errorf("error reading tip: %w", err)
	}
	return tip, nil
}

// lockTip reads the tip and locks the cursor row until the transaction ends.
func (w *Watcher) lockTip(ctx context.Context, tx pgx.Tx) (scannedBlock, error) {
	var tip scannedBlock
	if err := tx.QueryRow(ctx, selectTip+` FOR UPDATE OF c`).Scan(&tip.Number, &tip.Hash); err != nil {
		return scannedBlock{}, fmt.Errorf("error locking tip: %w", err)
	}
	return tip, nil
}

func setTip(ctx context.Context, tx pgx.Tx, blockNumber uint64) error {
	if _, err := tx.Exec(ctx, `UPDATE chain_cursor SET block_number = $1 WHERE id = 1`, blockNumber); err != nil {
		return fmt.Errorf("error moving tip to block %d: %w", blockNumber, err)
	}
	return nil
}

func insertScannedBlock(ctx context.Context, tx pgx.Tx, block chain.Block) error {
	if _, err := tx.Exec(ctx, `INSERT INTO scanned_blocks (number, hash, parent_hash) VALUES ($1, $2, $3)`,
		block.Number, block.Hash, block.ParentHash); err != nil {
		return fmt.Errorf("error inserting scanned block %d: %w", block.Number, err)
	}
	return nil
}
