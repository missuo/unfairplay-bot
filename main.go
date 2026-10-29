// Command unfairplay-bot downloads the current App Store build of an app with
// ipatool, decrypts it with UnfairPlay, and serves it through signed links.
package main

import (
	"context"
	"errors"
	"fmt"
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
	if len(os.Args) > 1 && os.Args[1] == "check" {
		os.Exit(runCheck())
	}
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
	if !supportsStripAccountData(cfg.UnfairplayBin) {
		return fmt.Errorf("%s does not support decrypt --strip-account-data (DrWhax/unfairplay#20); update unfairplay", cfg.UnfairplayBin)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	cache, err := OpenCache(filepath.Join(cfg.DataDir, "files"), cfg.CacheTTL, cfg.MaxCacheBytes)
	if err != nil {
		return err
	}
	store, err := NewStore(StoreOptions{
		StateDir: cfg.IpatoolStateDir, Passphrase: cfg.KeychainPassphrase, Purchase: cfg.Purchase,
		Country: cfg.StoreCountry, Credentials: cfg.AppleID,
		GuardPath: filepath.Join(cfg.DataDir, "auto-login.json"), MaxAutoLogins: cfg.MaxAutoLogins,
	})
	if err != nil {
		return err
	}
	var check func(context.Context) error
	if cfg.HealthAppID != 0 {
		check = func(ctx context.Context) error { return store.Check(ctx, cfg.HealthAppID) }
	}
	health := NewHealth(check, cfg.HealthInterval)
	svc := &Service{resolver: store, cache: cache, health: health, signer: Signer{key: cfg.SigningKey, base: cfg.PublicURL}}
	svc.manager, err = NewManager(ManagerOptions{
		Concurrency: cfg.Concurrency, DecryptConcurrency: cfg.DecryptConcurrency, MaxQueue: cfg.MaxQueue,
		Timeout: cfg.TaskTimeout, Retention: cfg.TaskRetention, WorkDir: filepath.Join(cfg.DataDir, "work"),
		OnFailure: svc.StoreFailed,
	}, pipeline{Store: store, Decrypter: Decrypter{Bin: cfg.UnfairplayBin, Model: cfg.ModelPath}}, cache)
	if err != nil {
		return err
	}
	manager := svc.manager

	srv := &Server{Service: svc, token: cfg.APIToken, versions: buildVersions()}
	if cfg.TelegramToken != "" {
		allow, err := OpenAllowlist(filepath.Join(cfg.DataDir, "telegram-allowlist.json"))
		if err != nil {
			return err
		}
		client := newTGClient(cfg.TelegramAPIURL, cfg.TelegramToken)
		bot := NewBot(client, svc, cfg.TelegramAdmins, allow, store, func() string {
			return statusText(manager, cache, health, allow, srv.versions, store.AccountEmail)
		})
		health.SetOnChange(bot.HealthChanged)
		botCtx, stopBot := context.WithCancel(context.Background())
		// The bot outlives the signal so it can keep reporting while tasks drain.
		defer stopBot()
		go bot.Run(botCtx, client)
		slog.Info("telegram bot enabled", "admins", cfg.TelegramAdmins)
	}
	httpServer := &http.Server{Addr: cfg.Listen, Handler: srv.Routes(), ReadHeaderTimeout: 10 * time.Second}

	healthCtx, stopHealth := context.WithCancel(context.Background())
	defer stopHealth()
	go health.Run(healthCtx)

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

// runCheck is the `check` subcommand used by deploy/update.sh to prove a new
// image can still talk to the App Store with the existing session before it
// replaces the running one. It never logs in by itself. Exit codes: 0 ok,
// 1 failed, 2 not configured.
func runCheck() int {
	cfg, err := LoadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if cfg.HealthAppID == 0 {
		fmt.Fprintln(os.Stderr, "UNFAIRPLAY_BOT_HEALTH_APP_ID is not set; skipping App Store check")
		return 2
	}
	store, err := NewStore(StoreOptions{
		StateDir: cfg.IpatoolStateDir, Passphrase: cfg.KeychainPassphrase, Country: cfg.StoreCountry,
		MaxAutoLogins: 0, // automatic login always refused
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := store.Check(ctx, cfg.HealthAppID); err != nil {
		fmt.Fprintln(os.Stderr, "App Store check failed:", classify(err).Error())
		return 1
	}
	fmt.Println("App Store check passed")
	return 0
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
