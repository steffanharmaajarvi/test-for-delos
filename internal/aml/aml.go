// Package aml is the boundary to the AML (risk scoring) provider.
package aml

import (
	"context"
	"errors"
	"sync/atomic"

	"delos/internal/chain"
)

type Risk string

const (
	RiskLow  Risk = "low"
	RiskHigh Risk = "high"
)

type Result struct {
	Risk Risk
	// Exchange is set when the address is a known exchange hot wallet. Money
	// must not be refunded to it automatically: it is shared by all the
	// exchange's customers.
	Exchange bool
}

// ErrUnavailable means the provider could not give an answer. The caller must
// retry later and must not treat it as either low or high risk.
var ErrUnavailable = errors.New("aml provider unavailable")

type Checker interface {
	Check(ctx context.Context, address chain.Address) (Result, error)
}

// Mock answers deterministically from static lists and can be switched off to
// simulate an outage.
type Mock struct {
	blacklist map[chain.Address]bool
	exchanges map[chain.Address]bool
	down      atomic.Bool
	calls     atomic.Int64
}

func NewMock(blacklist, exchanges []string) *Mock {
	m := &Mock{blacklist: map[chain.Address]bool{}, exchanges: map[chain.Address]bool{}}
	for _, a := range blacklist {
		m.blacklist[chain.NormalizeAddress(a)] = true
	}
	for _, a := range exchanges {
		m.exchanges[chain.NormalizeAddress(a)] = true
	}
	return m
}

func (m *Mock) Check(ctx context.Context, address chain.Address) (Result, error) {
	m.calls.Add(1)
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if m.down.Load() {
		return Result{}, ErrUnavailable
	}
	r := Result{Risk: RiskLow, Exchange: m.exchanges[address]}
	if m.blacklist[address] {
		r.Risk = RiskHigh
	}
	return r, nil
}

// SetAvailable switches the simulated outage off (true) or on (false).
func (m *Mock) SetAvailable(ok bool) { m.down.Store(!ok) }

// Available reports whether the mock currently answers.
func (m *Mock) Available() bool { return !m.down.Load() }

// Calls returns how many checks have been requested, for tests.
func (m *Mock) Calls() int64 { return m.calls.Load() }
