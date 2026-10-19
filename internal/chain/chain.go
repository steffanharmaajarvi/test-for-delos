// Package chain is the boundary between the service and Ethereum.
//
// The watcher only depends on Client. A JSON-RPC implementation would map
// HeadNumber to eth_blockNumber, BlockByNumber to eth_getBlockByNumber,
// TransferLogs to eth_getLogs{blockHash, address, topics[0]=Transfer} and
// TxSucceeded to eth_getTransactionReceipt(...).status == 1.
package chain

import (
	"context"
	"errors"
	"math/big"
	"strings"
)

// Address is a 0x-prefixed lowercase hex address.
type Address string

// NormalizeAddress lowercases the address. Comparing checksummed (mixed-case)
// and lowercase forms as plain strings is a classic matching bug.
func NormalizeAddress(s string) Address { return Address(strings.ToLower(strings.TrimSpace(s))) }

// ValidAddress reports whether s looks like a 20-byte hex address.
func ValidAddress(s string) bool {
	if len(s) != 42 || !strings.HasPrefix(s, "0x") {
		return false
	}
	for _, c := range strings.ToLower(s[2:]) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

type Hash string

type Block struct {
	Number     uint64
	Hash       Hash
	ParentHash Hash
}

// TransferLog is an ERC-20 Transfer(from, to, value) event.
type TransferLog struct {
	Token       Address // contract that emitted the event
	From        Address
	To          Address
	Amount      *big.Int
	TxHash      Hash
	LogIndex    uint
	BlockNumber uint64
	BlockHash   Hash
}

var ErrBlockNotFound = errors.New("block not found")

type Client interface {
	HeadNumber(ctx context.Context) (uint64, error)
	BlockByNumber(ctx context.Context, number uint64) (Block, error)
	// TransferLogs returns Transfer events emitted by token in the block with
	// the given hash. Querying by hash rather than number guarantees the logs
	// belong to exactly the block we validated.
	TransferLogs(ctx context.Context, blockHash Hash, token Address) ([]TransferLog, error)
	// TxSucceeded reports whether the transaction's receipt has status 1.
	TxSucceeded(ctx context.Context, txHash Hash) (bool, error)
}
