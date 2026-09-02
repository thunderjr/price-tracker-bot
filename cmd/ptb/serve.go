package main

import (
	"context"
	"fmt"
	"log/slog"
	"os/signal"
	"sync"
	"syscall"

	"github.com/thunderjr/price-tracker-bot/internal/browser"
	"github.com/thunderjr/price-tracker-bot/internal/config"
	"github.com/thunderjr/price-tracker-bot/internal/source"
	"github.com/thunderjr/price-tracker-bot/internal/source/amazon"
	"github.com/thunderjr/price-tracker-bot/internal/source/kabum"
	"github.com/thunderjr/price-tracker-bot/internal/source/meli"
	"github.com/thunderjr/price-tracker-bot/internal/store"
	"github.com/thunderjr/price-tracker-bot/internal/telegram"
	"github.com/thunderjr/price-tracker-bot/internal/tracker"
)

func runServe(cfg *config.Config) error {
	if err := cfg.RequireBot(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	chrome := browser.New(browser.Options{
		ExecPath:   cfg.ChromePath,
		ProfileDir: cfg.ChromeProfile,
		Logger:     slog.Default(),
	})
	defer chrome.Close()

	sources := []source.Source{meli.New(chrome), amazon.New(nil), kabum.New(nil)}
	trk := tracker.New(st, tracker.DefaultRules(cfg.DropThreshold, cfg.BestMoveThreshold), slog.Default(), sources...)

	bot, err := telegram.New(cfg, st, trk, slog.Default())
	if err != nil {
		return err
	}

	slog.Info("starting",
		"db", cfg.DBPath, "scan_interval", cfg.ScanInterval,
		"chats", len(cfg.AllowedChatIDs), "drop_threshold", cfg.DropThreshold)

	sched := tracker.NewScheduler(trk, st, bot, cfg.ScanInterval, slog.Default())

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); bot.Start(ctx) }()
	go func() { defer wg.Done(); sched.Run(ctx) }()

	<-ctx.Done()
	slog.Info("shutting down")
	wg.Wait()

	if err := ctx.Err(); err != nil && err != context.Canceled {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}
