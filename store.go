package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/byteness/keyring"
	cookiejar "github.com/juju/persistent-cookiejar"
	"github.com/majd/ipatool/v2/pkg/appstore"
	"github.com/majd/ipatool/v2/pkg/keychain"
	"github.com/majd/ipatool/v2/pkg/util/machine"
	"github.com/majd/ipatool/v2/pkg/util/operatingsystem"
	"github.com/schollz/progressbar/v3"
)

// Must match the ipatool CLI so a session created by `ipatool auth login`
// on the same host and state directory is reused.
const ipatoolKeychainService = "ipatool-auth.service"

const platform = appstore.PlatformIPhone

// Target is one exact App Store build. Key is "<trackId>:<externalVersionId>".
type Target struct {
	AppID             int64  `json:"app_id"`
	BundleID          string `json:"bundle_id,omitempty"`
	Name              string `json:"name,omitempty"`
	Version           string `json:"version,omitempty"`
	ExternalVersionID string `json:"external_version_id"`
	Key               string `json:"-"`
}

type Store struct {
	as       appstore.AppStore
	purchase bool
	country  string
	http     *http.Client
	creds    Credentials
	guard    *loginGuard
	mu       sync.Mutex // serializes keychain access and logins
	now      func() time.Time
}

type StoreOptions struct {
	StateDir, Passphrase string
	Purchase             bool
	Country              string
	Credentials          Credentials
	// GuardPath persists automatic-login failures across restarts.
	GuardPath     string
	MaxAutoLogins int
}

// Credentials, when set, let the service sign in by itself: on first start,
// when the session is lost, and when the token expires. They take precedence
// over the password ipatool saved in its keychain.
type Credentials struct {
	Email, Password string
}

func NewStore(o StoreOptions) (*Store, error) {
	stateDir := o.StateDir
	osys := operatingsystem.New()
	mach := machine.New(machine.Args{OS: osys})
	if stateDir == "" {
		stateDir = defaultIpatoolStateDir(mach.HomeDirectory())
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("ipatool state directory: %w", err)
	}
	jar, err := cookiejar.New(&cookiejar.Options{Filename: filepath.Join(stateDir, "cookies")})
	if err != nil {
		return nil, fmt.Errorf("open cookie jar: %w", err)
	}
	ring, err := keyring.Open(keyring.Config{
		AllowedBackends:          []keyring.BackendType{keyring.KeychainBackend, keyring.SecretServiceBackend, keyring.FileBackend},
		ServiceName:              ipatoolKeychainService,
		KeychainTrustApplication: true,
		FileDir:                  stateDir,
		FilePasswordFunc:         keyring.FixedStringPrompt(o.Passphrase),
	})
	if err != nil {
		return nil, fmt.Errorf("open keyring: %w", err)
	}
	as := appstore.NewAppStore(appstore.Args{
		CookieJar:       jar,
		OperatingSystem: osys,
		Machine:         mach,
		Keychain:        keychain.New(keychain.Args{Keyring: ring, Label: ipatoolKeychainService}),
	})
	guard, err := openLoginGuard(o.GuardPath, o.MaxAutoLogins)
	if err != nil {
		return nil, err
	}
	return &Store{
		as: as, purchase: o.Purchase, country: o.Country, creds: o.Credentials, guard: guard,
		http: &http.Client{Timeout: 15 * time.Second}, now: time.Now,
	}, nil
}

// defaultIpatoolStateDir mirrors ipatool's lookup without migrating anything.
func defaultIpatoolStateDir(home string) string {
	legacy := filepath.Join(home, ".ipatool")
	if info, err := os.Stat(legacy); err == nil && info.IsDir() {
		return legacy
	}
	for _, name := range []string{"XDG_STATE_HOME", "XDG_DATA_HOME"} {
		if base := os.Getenv(name); filepath.IsAbs(base) {
			return filepath.Join(base, "ipatool")
		}
	}
	return legacy
}

var errNoCredentials = &TaskError{Code: "auth_required", Message: "not signed in and no Apple ID credentials are configured; set UNFAIRPLAY_BOT_APPLE_ID_EMAIL and UNFAIRPLAY_BOT_APPLE_ID_PASSWORD or run `ipatool auth login` on the server"}

// credentialsLocked picks the configured credentials, else the ones ipatool
// saved with the session.
func (s *Store) credentialsLocked() (Credentials, bool) {
	if s.creds.Email != "" && s.creds.Password != "" {
		return s.creds, true
	}
	info, err := s.as.AccountInfo()
	if err != nil || info.Account.Email == "" || info.Account.Password == "" {
		return Credentials{}, false
	}
	return Credentials{Email: info.Account.Email, Password: info.Account.Password}, true
}

// loginLocked signs in; automatic attempts respect the backoff.
func (s *Store) loginLocked(code string, automatic bool) (appstore.Account, error) {
	creds, ok := s.credentialsLocked()
	if !ok {
		return appstore.Account{}, errNoCredentials
	}
	if automatic {
		if err := s.guard.allow(s.now()); err != nil {
			return appstore.Account{}, err
		}
	}
	out, err := s.as.Login(appstore.LoginInput{Email: creds.Email, Password: creds.Password, AuthCode: code})
	if err != nil {
		if automatic {
			s.guard.failed(s.now())
		}
		if errors.Is(err, appstore.ErrAuthCodeRequired) {
			return appstore.Account{}, err
		}
		return appstore.Account{}, &TaskError{Code: "auth_required", Message: "Apple ID login failed: " + err.Error()}
	}
	s.guard.succeeded()
	slog.Info("signed in to the App Store", "email", creds.Email, "automatic", automatic)
	return out.Account, nil
}

// account returns the signed-in account, signing in first when there is no
// usable session and credentials are available.
func (s *Store) account() (appstore.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, err := s.as.AccountInfo()
	if err == nil && info.Account.PasswordToken != "" {
		return info.Account, nil
	}
	if s.creds.Email == "" {
		return appstore.Account{}, &TaskError{Code: "auth_required", Message: "no usable ipatool session; run `ipatool auth login` on the server or configure Apple ID credentials"}
	}
	return s.loginLocked("", true)
}

func (s *Store) relogin(stale appstore.Account) (appstore.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Another task may already have refreshed the token.
	if info, err := s.as.AccountInfo(); err == nil && info.Account.PasswordToken != stale.PasswordToken {
		return info.Account, nil
	}
	acc, err := s.loginLocked("", true)
	if errors.Is(err, appstore.ErrAuthCodeRequired) {
		return acc, &TaskError{Code: "auth_required", Message: "Apple asked for a 2FA code; send /login to the bot"}
	}
	return acc, err
}

// StartLogin is the admin's manual sign-in. For a 2FA account Apple sends a
// code to trusted devices and needsCode is returned; FinishLogin completes it.
func (s *Store) StartLogin() (email string, needsCode bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	creds, _ := s.credentialsLocked()
	_, err = s.loginLocked("", false)
	if errors.Is(err, appstore.ErrAuthCodeRequired) {
		return creds.Email, true, nil
	}
	return creds.Email, false, err
}

func (s *Store) FinishLogin(code string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	creds, _ := s.credentialsLocked()
	_, err := s.loginLocked(code, false)
	return creds.Email, err
}

// call runs fn with the current account, refreshing an expired token and
// acquiring a license once when allowed, like `ipatool download --purchase`.
func (s *Store) call(app *appstore.App, fn func(appstore.Account) error) error {
	acc, err := s.account()
	if err != nil {
		return err
	}
	purchased := false
	for attempt := 0; ; attempt++ {
		err = fn(acc)
		switch {
		case err == nil || attempt == 2:
			return err
		case errors.Is(err, appstore.ErrPasswordTokenExpired):
			if acc, err = s.relogin(acc); err != nil {
				return err
			}
		case errors.Is(err, appstore.ErrLicenseRequired) && s.purchase && !purchased && app.ID != 0:
			err = s.as.Purchase(appstore.PurchaseInput{Account: acc, App: *app, Platform: platform})
			if err != nil && !errors.Is(err, appstore.ErrLicenseAlreadyExists) {
				return err
			}
			purchased = true
		default:
			return err
		}
	}
}

// Download fetches exactly t's build into dir and returns the IPA path.
func (s *Store) Download(ctx context.Context, t Target, dir string, report func(Stage, *Progress)) (string, error) {
	// The bar must stay "visible": an invisible bar skips state updates.
	// It renders into io.Discard, rarely.
	bar := progressbar.NewOptions64(1, progressbar.OptionSetWriter(io.Discard), progressbar.OptionThrottle(time.Second))
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		watchProgress(bar, done, report)
	}()

	app := appstore.App{ID: t.AppID, BundleID: t.BundleID, Name: t.Name}
	var out appstore.DownloadOutput
	err := s.call(&app, func(acc appstore.Account) (err error) {
		out, err = s.as.Download(appstore.DownloadInput{
			Context: ctx, Account: acc, App: app, Platform: platform,
			OutputPath:        filepath.Join(dir, "encrypted.ipa"),
			ExternalVersionID: t.ExternalVersionID,
			Progress:          bar,
		})
		return err
	})
	close(done)
	wg.Wait()
	if err != nil {
		return "", err
	}
	report(StagePatching, nil)
	if err := s.as.ReplicateSinf(appstore.ReplicateSinfInput{Sinfs: out.Sinfs, PackagePath: out.DestinationPath}); err != nil {
		return "", fmt.Errorf("replicate sinf: %w", err)
	}
	return out.DestinationPath, nil
}

// watchProgress samples the (invisible) progress bar ipatool writes into.
// After the transfer completes ipatool still rewrites the zip to add
// metadata, which is reported as the patching stage.
func watchProgress(bar *progressbar.ProgressBar, done <-chan struct{}, report func(Stage, *Progress)) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	var lastBytes int64
	var lastAt time.Time
	var speed float64
	for {
		select {
		case <-done:
			return
		case now := <-ticker.C:
			// State() is read under the bar's lock; derive the total from it
			// instead of the unsynchronized GetMax64.
			st := bar.State()
			if st.CurrentPercent <= 0 || st.CurrentBytes <= 0 {
				continue
			}
			current := int64(st.CurrentBytes)
			total := int64(st.CurrentBytes/st.CurrentPercent + 0.5)
			if current >= total {
				report(StagePatching, nil)
				continue
			}
			if !lastAt.IsZero() && current >= lastBytes {
				instant := float64(current-lastBytes) / now.Sub(lastAt).Seconds()
				if speed == 0 {
					speed = instant
				} else {
					speed = 0.3*instant + 0.7*speed
				}
			}
			lastBytes, lastAt = current, now
			p := &Progress{Bytes: current, Total: total, Percent: float64(current) * 100 / float64(total), SpeedBps: int64(speed)}
			if speed > 0 {
				p.ETASeconds = int64(float64(total-current) / speed)
			}
			report(StageDownloading, p)
		}
	}
}

// classify maps errors to stable codes for API clients.
func classify(err error) *TaskError {
	var te *TaskError
	switch {
	case errors.As(err, &te):
		return te
	case errors.Is(err, context.DeadlineExceeded):
		return &TaskError{Code: "timeout", Message: err.Error()}
	case errors.Is(err, context.Canceled):
		return &TaskError{Code: "canceled", Message: "task was canceled"}
	case errors.Is(err, appstore.ErrAuthCodeRequired):
		return &TaskError{Code: "auth_required", Message: "Apple requires a 2FA code; run `ipatool auth login` on the server"}
	case errors.Is(err, appstore.ErrAppNotFound):
		return &TaskError{Code: "not_found", Message: err.Error()}
	case errors.Is(err, appstore.ErrLicenseRequired):
		return &TaskError{Code: "license_required", Message: "the account does not own this app (paid apps must be bought first)"}
	case errors.Is(err, appstore.ErrSubscriptionRequired):
		return &TaskError{Code: "subscription_required", Message: err.Error()}
	case errors.Is(err, appstore.ErrTemporarilyUnavailable):
		return &TaskError{Code: "unavailable", Message: err.Error()}
	}
	return &TaskError{Code: "store_error", Message: err.Error()}
}

// Check makes the same App Store request a download starts with
// (download-product, via ListVersions) for a known app, without downloading.
func (s *Store) Check(ctx context.Context, appID int64) error {
	app := appstore.App{ID: appID}
	return s.call(&app, func(acc appstore.Account) error {
		_, err := s.as.ListVersions(appstore.ListVersionsInput{Context: ctx, Account: acc, App: app, Platform: platform})
		return err
	})
}
