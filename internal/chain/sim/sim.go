// Package sim is an Ethereum simulator implementing chain.Client.
//
// State lives in PostgreSQL (sim_* tables) so that the chain survives a
// service restart just like a real one would. Every Transfer call mines a new
// block containing that one transaction; Mine adds empty blocks; Reorg replaces
// the last blocks with a longer fork.
package sim

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"delos/internal/chain"
)

const schema = `
CREATE TABLE IF NOT EXISTS sim_blocks (
    number      BIGINT PRIMARY KEY,
    hash        TEXT NOT NULL UNIQUE,
    parent_hash TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sim_logs (
    block_hash   TEXT    NOT NULL REFERENCES sim_blocks (hash) ON DELETE CASCADE,
    log_index    INTEGER NOT NULL,
    tx_hash      TEXT    NOT NULL,
    token        TEXT    NOT NULL,
    from_address TEXT    NOT NULL,
    to_address   TEXT    NOT NULL,
    amount       TEXT    NOT NULL, -- uint256 as decimal string
    succeeded    BOOLEAN NOT NULL,
    PRIMARY KEY (block_hash, log_index)
);
CREATE INDEX IF NOT EXISTS sim_logs_tx ON sim_logs (tx_hash);
`

// lockID serialises writers to the simulated chain.
const lockID = 7_311_002

type Simulator struct {
	pool *pgxpool.Pool
}

// New creates the simulator tables and the genesis block if they are missing.
func New(ctx context.Context, pool *pgxpool.Pool) (*Simulator, error) {
	s := &Simulator{pool: pool}
	err := s.write(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, schema); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO sim_blocks (number, hash, parent_hash) VALUES (0, $1, $2)
			ON CONFLICT DO NOTHING`, randomHash(), zeroHash)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("error initialising simulator: %w", err)
	}
	return s, nil
}

// TransferRequest describes a simulated ERC-20 Transfer.
type TransferRequest struct {
	Token  chain.Address
	From   chain.Address
	To     chain.Address
	Amount *big.Int
	// Failed simulates a reverted transaction whose log is nevertheless
	// returned by the node, to exercise the receipt-status check.
	Failed bool
}

// Transfer mines a new block containing the transfer and returns its log.
func (s *Simulator) Transfer(ctx context.Context, r TransferRequest) (chain.TransferLog, error) {
	if r.Amount == nil || r.Amount.Sign() <= 0 {
		return chain.TransferLog{}, errors.New("amount must be positive")
	}
	log := chain.TransferLog{
		Token:    r.Token,
		From:     r.From,
		To:       r.To,
		Amount:   new(big.Int).Set(r.Amount),
		TxHash:   chain.Hash(randomHash()),
		LogIndex: 0,
	}
	err := s.write(ctx, func(tx pgx.Tx) error {
		b, err := mine(ctx, tx)
		if err != nil {
			return err
		}
		log.BlockNumber, log.BlockHash = b.Number, b.Hash
		return insertLog(ctx, tx, log, !r.Failed)
	})
	return log, err
}

// Mine appends n empty blocks and returns the new head.
func (s *Simulator) Mine(ctx context.Context, n int) (uint64, error) {
	var head uint64
	err := s.write(ctx, func(tx pgx.Tx) error {
		for range n {
			b, err := mine(ctx, tx)
			if err != nil {
				return err
			}
			head = b.Number
		}
		return nil
	})
	return head, err
}

// Reorg drops the last depth blocks and mines depth+1 new ones in their place,
// so the fork becomes the longest chain. With reinclude the dropped
// transactions reappear in the first block of the fork (same tx hash, new block
// hash), otherwise they vanish as if never mined.
func (s *Simulator) Reorg(ctx context.Context, depth int, reinclude bool) error {
	if depth <= 0 {
		return errors.New("depth must be positive")
	}
	return s.write(ctx, func(tx pgx.Tx) error {
		var head uint64
		if err := tx.QueryRow(ctx, `SELECT max(number) FROM sim_blocks`).Scan(&head); err != nil {
			return err
		}
		if uint64(depth) > head {
			return fmt.Errorf("cannot reorg %d blocks, head is %d", depth, head)
		}
		forkPoint := head - uint64(depth)

		type droppedLog struct {
			log       chain.TransferLog
			succeeded bool
		}
		var dropped []droppedLog
		if reinclude {
			rows, err := tx.Query(ctx, `
				SELECT l.tx_hash, l.token, l.from_address, l.to_address, l.amount, l.succeeded
				FROM sim_logs l JOIN sim_blocks b ON b.hash = l.block_hash
				WHERE b.number > $1 ORDER BY b.number, l.log_index`, forkPoint)
			if err != nil {
				return err
			}
			for rows.Next() {
				var d droppedLog
				var amount string
				if err := rows.Scan(&d.log.TxHash, &d.log.Token, &d.log.From, &d.log.To, &amount, &d.succeeded); err != nil {
					rows.Close()
					return err
				}
				d.log.Amount, _ = new(big.Int).SetString(amount, 10)
				dropped = append(dropped, d)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
		}

		if _, err := tx.Exec(ctx, `DELETE FROM sim_blocks WHERE number > $1`, forkPoint); err != nil {
			return err
		}
		for i := 0; i <= depth; i++ {
			b, err := mine(ctx, tx)
			if err != nil {
				return err
			}
			if i != 0 {
				continue
			}
			for idx, d := range dropped {
				d.log.BlockNumber, d.log.BlockHash, d.log.LogIndex = b.Number, b.Hash, uint(idx)
				if err := insertLog(ctx, tx, d.log, d.succeeded); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (s *Simulator) HeadNumber(ctx context.Context) (uint64, error) {
	var head uint64
	err := s.pool.QueryRow(ctx, `SELECT max(number) FROM sim_blocks`).Scan(&head)
	return head, err
}

func (s *Simulator) BlockByNumber(ctx context.Context, number uint64) (chain.Block, error) {
	b := chain.Block{Number: number}
	err := s.pool.QueryRow(ctx, `SELECT hash, parent_hash FROM sim_blocks WHERE number = $1`, number).
		Scan(&b.Hash, &b.ParentHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return chain.Block{}, chain.ErrBlockNotFound
	}
	return b, err
}

// TransferLogs filters by token like eth_getLogs does with the address param.
func (s *Simulator) TransferLogs(ctx context.Context, blockHash chain.Hash, token chain.Address) ([]chain.TransferLog, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT b.number, l.log_index, l.tx_hash, l.token, l.from_address, l.to_address, l.amount
		FROM sim_logs l JOIN sim_blocks b ON b.hash = l.block_hash
		WHERE l.block_hash = $1 AND l.token = $2
		ORDER BY l.log_index`, blockHash, token)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []chain.TransferLog
	for rows.Next() {
		l := chain.TransferLog{BlockHash: blockHash}
		var amount string
		if err := rows.Scan(&l.BlockNumber, &l.LogIndex, &l.TxHash, &l.Token, &l.From, &l.To, &amount); err != nil {
			return nil, err
		}
		var ok bool
		if l.Amount, ok = new(big.Int).SetString(amount, 10); !ok {
			return nil, fmt.Errorf("bad amount %q in sim_logs", amount)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Simulator) TxSucceeded(ctx context.Context, txHash chain.Hash) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `
		SELECT bool_and(succeeded) FROM sim_logs WHERE tx_hash = $1 HAVING count(*) > 0`, txHash).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("receipt for %s not found", txHash)
	}
	return ok, err
}

func (s *Simulator) write(ctx context.Context, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, lockID); err != nil {
			return err
		}
		return fn(tx)
	})
}

func mine(ctx context.Context, tx pgx.Tx) (chain.Block, error) {
	var parent chain.Block
	if err := tx.QueryRow(ctx, `SELECT number, hash FROM sim_blocks ORDER BY number DESC LIMIT 1`).
		Scan(&parent.Number, &parent.Hash); err != nil {
		return chain.Block{}, err
	}
	b := chain.Block{Number: parent.Number + 1, Hash: chain.Hash(randomHash()), ParentHash: parent.Hash}
	_, err := tx.Exec(ctx, `INSERT INTO sim_blocks (number, hash, parent_hash) VALUES ($1, $2, $3)`,
		b.Number, b.Hash, b.ParentHash)
	return b, err
}

func insertLog(ctx context.Context, tx pgx.Tx, l chain.TransferLog, succeeded bool) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO sim_logs (block_hash, log_index, tx_hash, token, from_address, to_address, amount, succeeded)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		l.BlockHash, l.LogIndex, l.TxHash, l.Token, l.From, l.To, l.Amount.String(), succeeded)
	return err
}

const zeroHash = "0x0000000000000000000000000000000000000000000000000000000000000000"

func randomHash() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return "0x" + hex.EncodeToString(b[:])
}
