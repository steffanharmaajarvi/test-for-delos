// Package address issues deposit addresses.
package address

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"

	"delos/internal/chain"
)

// Generator maps a monotonic index to a deposit address. In production this is
// BIP-32 derivation from an xpub (m/44'/60'/0'/0/{index}): the API knows only
// the public key and the private key exists only inside the signer.
type Generator interface {
	Address(index uint64) chain.Address
}

// Deterministic derives a pseudo-address from a seed and the index. There is
// no private key behind it; it only has to be unique and stable.
type Deterministic struct {
	Seed []byte
}

func (d Deterministic) Address(index uint64) chain.Address {
	h := sha256.New()
	h.Write(d.Seed)
	_ = binary.Write(h, binary.BigEndian, index)
	sum := h.Sum(nil)
	return chain.Address("0x" + hex.EncodeToString(sum[:20]))
}
