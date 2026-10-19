// Package config reads service settings from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"delos/internal/chain"
	"delos/internal/partner"
)

// MainnetUSDT is the Tether USD contract on Ethereum mainnet.
const MainnetUSDT = "0xdac17f958d2ee523a2206206994597c13d831ec7"

type Config struct {
	DatabaseURL     string
	HTTPAddr        string
	USDTContract    chain.Address
	Confirmations   uint64
	PollInterval    time.Duration
	AddressSeed     string
	AMLBlacklist    []string
	AMLExchanges    []string
	AMLRetryBackoff time.Duration
	AMLMaxBackoff   time.Duration
	Partners        []partner.Seed
	SimEnabled      bool
}

func FromEnv() (Config, error) {
	c := Config{
		DatabaseURL:  os.Getenv("DATABASE_URL"),
		HTTPAddr:     env("HTTP_ADDR", ":8080"),
		USDTContract: chain.NormalizeAddress(env("USDT_CONTRACT", MainnetUSDT)),
		AddressSeed:  env("ADDRESS_SEED", "dev-seed"),
		AMLBlacklist: list(os.Getenv("AML_BLACKLIST")),
		AMLExchanges: list(os.Getenv("AML_EXCHANGES")),
	}
	var errs []error
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	if !chain.ValidAddress(string(c.USDTContract)) {
		errs = append(errs, fmt.Errorf("USDT_CONTRACT %q is not an address", c.USDTContract))
	}
	var err error
	if c.Confirmations, err = strconv.ParseUint(env("CONFIRMATIONS", "12"), 10, 64); err != nil || c.Confirmations == 0 {
		errs = append(errs, fmt.Errorf("CONFIRMATIONS must be a positive integer"))
	}
	c.PollInterval, err = duration("POLL_INTERVAL", "1s")
	errs = append(errs, err)
	c.AMLRetryBackoff, err = duration("AML_RETRY_BACKOFF", "5s")
	errs = append(errs, err)
	c.AMLMaxBackoff, err = duration("AML_MAX_BACKOFF", "5m")
	errs = append(errs, err)
	c.Partners, err = parsePartners(os.Getenv("PARTNERS"))
	errs = append(errs, err)
	c.SimEnabled, err = strconv.ParseBool(env("SIM_ENABLED", "false"))
	if err != nil {
		errs = append(errs, fmt.Errorf("SIM_ENABLED: %w", err))
	}
	return c, errors.Join(errs...)
}

// parsePartners reads "id:name:api_key:fee_bps" entries separated by commas.
func parsePartners(s string) ([]partner.Seed, error) {
	var out []partner.Seed
	for _, item := range list(s) {
		parts := strings.Split(item, ":")
		if len(parts) != 4 {
			return nil, fmt.Errorf("PARTNERS entry %q: want id:name:api_key:fee_bps", item)
		}
		bps, err := strconv.Atoi(parts[3])
		if err != nil || bps < 0 || bps > 10_000 {
			return nil, fmt.Errorf("PARTNERS entry %q: fee_bps must be 0..10000", parts[0])
		}
		if parts[0] == "" || len(parts[2]) < 16 {
			return nil, fmt.Errorf("PARTNERS entry %q: id is required and api_key must be at least 16 chars", parts[0])
		}
		out = append(out, partner.Seed{
			Partner: partner.Partner{ID: parts[0], Name: parts[1], FeeBps: bps},
			APIKey:  parts[2],
		})
	}
	return out, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func duration(k, def string) (time.Duration, error) {
	d, err := time.ParseDuration(env(k, def))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", k, err)
	}
	return d, nil
}

func list(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
