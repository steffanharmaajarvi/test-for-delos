// Package partner stores merchants and authenticates them by API key.
package partner

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Partner struct {
	ID     string
	Name   string
	FeeBps int // processing fee in basis points, 100 = 1%
}

// Seed is a partner definition from configuration.
type Seed struct {
	Partner
	APIKey string
}

var ErrUnauthorized = errors.New("unauthorized")

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Upsert creates or updates partners from configuration. Only the hash of the
// API key is stored.
func (s *Store) Upsert(ctx context.Context, seeds []Seed) error {
	for _, p := range seeds {
		_, err := s.pool.Exec(ctx, `
			INSERT INTO partners (id, name, api_key_hash, fee_bps) VALUES ($1, $2, $3, $4)
			ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, api_key_hash = EXCLUDED.api_key_hash,
				fee_bps = EXCLUDED.fee_bps`,
			p.ID, p.Name, hashKey(p.APIKey), p.FeeBps)
		if err != nil {
			return fmt.Errorf("error upserting partner %s: %w", p.ID, err)
		}
	}
	return nil
}

// Authenticate resolves an API key to its partner. Lookup goes by the key's
// hash, so there is no string comparison to time.
func (s *Store) Authenticate(ctx context.Context, apiKey string) (Partner, error) {
	if apiKey == "" {
		return Partner{}, ErrUnauthorized
	}
	var p Partner
	err := s.pool.QueryRow(ctx, `SELECT id, name, fee_bps FROM partners WHERE api_key_hash = $1`, hashKey(apiKey)).
		Scan(&p.ID, &p.Name, &p.FeeBps)
	if errors.Is(err, pgx.ErrNoRows) {
		return Partner{}, ErrUnauthorized
	}
	return p, err
}

func hashKey(k string) []byte {
	h := sha256.Sum256([]byte(k))
	return h[:]
}
