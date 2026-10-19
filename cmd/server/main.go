package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"delos/internal/app"
	"delos/internal/config"
	"delos/internal/db"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	a, err := app.New(ctx, cfg, pool, log)
	if err != nil {
		return err
	}
	if cfg.SimEnabled {
		log.Warn("chain simulator and AML mock control API are enabled")
	}
	return a.Run(ctx)
}
