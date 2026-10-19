// Package app wires the components together. main and the end-to-end tests
// build the service the same way.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"delos/internal/address"
	"delos/internal/aml"
	"delos/internal/api"
	"delos/internal/chain/sim"
	"delos/internal/config"
	"delos/internal/db"
	"delos/internal/invoice"
	"delos/internal/partner"
	"delos/internal/processor"
	"delos/internal/watcher"
)

type App struct {
	cfg       config.Config
	log       *slog.Logger
	Handler   http.Handler
	Sim       *sim.Simulator
	AML       *aml.Mock
	Watcher   *watcher.Watcher
	Processor *processor.Processor
}

// New migrates the database, seeds partners and builds all components.
func New(ctx context.Context, cfg config.Config, pool *pgxpool.Pool, log *slog.Logger) (*App, error) {
	if err := db.Migrate(ctx, pool); err != nil {
		return nil, fmt.Errorf("error migrating database: %w", err)
	}
	partners := partner.NewStore(pool)
	if err := partners.Upsert(ctx, cfg.Partners); err != nil {
		return nil, err
	}

	// The simulator is the only chain.Client implementation; a JSON-RPC client
	// would be plugged in here.
	simulator, err := sim.New(ctx, pool)
	if err != nil {
		return nil, err
	}
	amlMock := aml.NewMock(cfg.AMLBlacklist, cfg.AMLExchanges)

	invoices := invoice.NewService(pool, address.Deterministic{Seed: []byte(cfg.AddressSeed)})
	mux := http.NewServeMux()
	api.NewServer(pool, partners, invoices, log).Register(mux)
	if cfg.SimEnabled {
		api.NewSimServer(simulator, amlMock, cfg.USDTContract, log).Register(mux)
	}

	return &App{
		cfg:     cfg,
		log:     log,
		Handler: mux,
		Sim:     simulator,
		AML:     amlMock,
		Watcher: watcher.New(pool, simulator, watcher.Config{
			Token:         cfg.USDTContract,
			Confirmations: cfg.Confirmations,
		}, log.With("component", "watcher")),
		Processor: processor.New(pool, amlMock, processor.Config{
			RetryBackoff: cfg.AMLRetryBackoff,
			MaxBackoff:   cfg.AMLMaxBackoff,
		}, log.With("component", "processor")),
	}, nil
}

// Run serves HTTP and runs the background workers until ctx is cancelled.
func (a *App) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              a.cfg.HTTPAddr,
		Handler:           a.Handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		a.log.Info("http listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	g.Go(func() error {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	})
	g.Go(func() error { return a.Watcher.Run(ctx, a.cfg.PollInterval) })
	g.Go(func() error { return a.Processor.Run(ctx, a.cfg.PollInterval) })
	return g.Wait()
}
