// Command unfairplay-bot downloads the current App Store build of an app with
// ipatool, decrypts it with UnfairPlay, and serves it through signed links.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	if cfg.UnfairplayBin, err = exec.LookPath(cfg.UnfairplayBin); err != nil {
		return err
	}
	if _, err := os.Stat(cfg.ModelPath); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	cache, err := OpenCache(filepath.Join(cfg.DataDir, "files"), cfg.CacheTTL, cfg.MaxCacheBytes)
	if err != nil {
		return err
	}
	store, err := NewStore(cfg.IpatoolStateDir, cfg.KeychainPassphrase, cfg.Purchase, cfg.StoreCountry)
	if err != nil {
		return err
	}
	manager, err := NewManager(ManagerOptions{
		Concurrency: cfg.Concurrency, DecryptConcurrency: cfg.DecryptConcurrency, MaxQueue: cfg.MaxQueue,
		Timeout: cfg.TaskTimeout, Retention: cfg.TaskRetention, WorkDir: filepath.Join(cfg.DataDir, "work"),
	}, pipeline{Store: store, Decrypter: Decrypter{Bin: cfg.UnfairplayBin, Model: cfg.ModelPath}}, cache)
	if err != nil {
		return err
	}

	svc := &Service{resolver: store, manager: manager, cache: cache, signer: Signer{key: cfg.SigningKey, base: cfg.PublicURL}}
	srv := &Server{Service: svc, token: cfg.APIToken, versions: buildVersions()}
	if cfg.TelegramToken != "" {
		allow, err := OpenAllowlist(filepath.Join(cfg.DataDir, "telegram-allowlist.json"))
		if err != nil {
			return err
		}
		client := newTGClient(cfg.TelegramAPIURL, cfg.TelegramToken)
		bot := NewBot(client, svc, cfg.TelegramAdmins, allow, func() string {
			return statusText(manager, cache, allow, srv.versions, store.AccountEmail)
		})
		botCtx, stopBot := context.WithCancel(context.Background())
		// The bot outlives the signal so it can keep reporting while tasks drain.
		defer stopBot()
		go bot.Run(botCtx, client)
		slog.Info("telegram bot enabled", "admins", cfg.TelegramAdmins)
	}
	httpServer := &http.Server{Addr: cfg.Listen, Handler: srv.Routes(), ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cache.Sweep()
				manager.Prune()
			}
		}
	}()

	errc := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.Listen, "versions", srv.versions)
		errc <- httpServer.ListenAndServe()
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	// Keep serving status, events, and downloads while queued and running
	// tasks finish; new tasks are refused.
	slog.Info("draining tasks", "status", manager.Status())
	drainCtx, cancel := context.WithTimeout(context.Background(), cfg.DrainTimeout)
	defer cancel()
	if err := manager.Drain(drainCtx); err != nil {
		slog.Warn("drain timed out; remaining tasks were canceled")
	}
	shutdownCtx, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	if err := httpServer.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// buildVersions reports the dependency versions compiled in, plus the
// UnfairPlay ref the image was built from.
func buildVersions() map[string]string {
	v := map[string]string{"unfairplay": os.Getenv("UNFAIRPLAY_REF")}
	if info, ok := debug.ReadBuildInfo(); ok {
		v["unfairplay-bot"] = info.Main.Version
		for _, dep := range info.Deps {
			if dep.Path == "github.com/majd/ipatool/v2" {
				v["ipatool"] = dep.Version
			}
		}
	}
	return v
}
