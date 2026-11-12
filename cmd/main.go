package main

import (
	"context"
	"flag"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"mattermost-locker/internal/announce"
	"mattermost-locker/internal/config"
	"mattermost-locker/internal/notify"
	"mattermost-locker/internal/store"
	"mattermost-locker/internal/textfmt"
	"mattermost-locker/internal/webserver"
	"mattermost-locker/internal/webserver/command"
)

func main() {
	logger := slog.New(
		slog.NewJSONHandler(log.Writer(), &slog.HandlerOptions{
			Level: slog.LevelInfo,
		}),
	)
	slog.SetDefault(logger)

	if err := run(); err != nil {
		slog.Error("Fatal error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "config.json", "path to the JSON configuration file")
	flag.Parse()

	// Invalid configuration aborts startup.
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(
		context.Background(),
		syscall.SIGINT,
		syscall.SIGTERM,
	)
	defer stop()

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	// Merge configured locks into the database. Existing locks keep their
	// holder and lock time; only configuration (timeout) is updated.
	specs := make([]store.LockSpec, len(cfg.Locks))
	for i, l := range cfg.Locks {
		specs[i] = store.LockSpec{Name: l.Name, Timeout: l.Timeout}
	}
	summary, err := st.SyncLocks(ctx, specs)
	if err != nil {
		return err
	}
	slog.Info("Locks synchronized with configuration",
		"added", summary.Added,
		"timeout_changed", summary.Updated,
		"removed", summary.Removed,
		"total", len(specs),
	)

	webhook := notify.NewWebhook(cfg.WebhookURL)
	formatter := textfmt.Formatter{Loc: cfg.Location(), Now: time.Now}
	announcer := announce.New(webhook, formatter)
	st.SetExpiryHook(announcer.TimedOut)

	// Release locks that timed out while we were offline (using the timeouts
	// just loaded from configuration) and announce them. Locks that have not
	// timed out are untouched.
	if err := st.ExpireDue(ctx); err != nil {
		return err
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		st.RunExpiryLoop(ctx, cfg.ExpiryCheckInterval)
	}()

	handlers := command.New(st, announcer, formatter, command.Options{
		AuditDefaultLimit: cfg.AuditDefaultLimit,
		AuditMaxLimit:     cfg.AuditMaxLimit,
	})
	srv := webserver.New(cfg.WebserverPort, cfg.SlashCommandToken, handlers)
	if err := srv.Start(); err != nil {
		return err
	}

	<-ctx.Done()
	slog.Info("Shutdown signal received")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("Graceful shutdown failed", "error", err)
	}
	wg.Wait() // expiry loop exits when ctx is cancelled
	if err := webhook.Close(shutdownCtx); err != nil {
		slog.Error("Flushing notifications failed", "error", err)
	}

	slog.Info("Server stopped")
	return nil
}
