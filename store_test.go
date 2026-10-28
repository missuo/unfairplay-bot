package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/majd/ipatool/v2/pkg/appstore"
)

// fakeAppStore implements the account parts of appstore.AppStore.
type fakeAppStore struct {
	appstore.AppStore
	account  appstore.Account
	signedIn bool
	loginErr error
	logins   int

	purchaseErrs []error // returned in order, then nil
	purchases    int
}

func (f *fakeAppStore) Purchase(appstore.PurchaseInput) error {
	f.purchases++
	if len(f.purchaseErrs) > 0 {
		err := f.purchaseErrs[0]
		f.purchaseErrs = f.purchaseErrs[1:]
		return err
	}
	return nil
}

func (f *fakeAppStore) AccountInfo() (appstore.AccountInfoOutput, error) {
	if !f.signedIn {
		return appstore.AccountInfoOutput{}, errors.New("not found")
	}
	return appstore.AccountInfoOutput{Account: f.account}, nil
}

func (f *fakeAppStore) Login(in appstore.LoginInput) (appstore.LoginOutput, error) {
	f.logins++
	if f.loginErr != nil {
		return appstore.LoginOutput{}, f.loginErr
	}
	f.signedIn = true
	f.account = appstore.Account{Email: in.Email, Password: in.Password, PasswordToken: "token"}
	return appstore.LoginOutput{Account: f.account}, nil
}

func newGuardedStore(t *testing.T, path string, fake *fakeAppStore, now *time.Time) *Store {
	t.Helper()
	guard, err := openLoginGuard(path, 3)
	if err != nil {
		t.Fatal(err)
	}
	return &Store{as: fake, guard: guard, creds: Credentials{Email: "me@example.com", Password: "pw"},
		now: func() time.Time { return *now }}
}

func TestStoreSignsInWithConfiguredCredentials(t *testing.T) {
	now := time.Now()
	fake := &fakeAppStore{}
	s := newGuardedStore(t, "", fake, &now)
	acc, err := s.account()
	if err != nil || acc.Email != "me@example.com" || fake.logins != 1 {
		t.Fatalf("account() = %+v, %v; logins %d", acc, err, fake.logins)
	}
	// A valid session is reused without logging in again.
	if _, err := s.account(); err != nil || fake.logins != 1 {
		t.Fatalf("second account(): %v, logins %d", err, fake.logins)
	}
}

func TestStoreAutoLoginBackoffAndLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auto-login.json")
	now := time.Now()
	fake := &fakeAppStore{loginErr: errors.New("bad password")}
	s := newGuardedStore(t, path, fake, &now)

	for i := 1; i <= 3; i++ {
		if _, err := s.account(); err == nil {
			t.Fatal("expected failure")
		}
		if fake.logins != i {
			t.Fatalf("attempt %d: logins %d", i, fake.logins)
		}
		// Within the backoff no further attempt reaches Apple.
		if _, err := s.account(); err == nil || fake.logins != i {
			t.Fatalf("attempt %d retried during backoff (logins %d)", i, fake.logins)
		}
		now = now.Add(autoLoginBackoff)
	}
	// Limit reached: no more automatic attempts, even after a long time or a
	// restart.
	now = now.Add(24 * time.Hour)
	if _, err := s.account(); err == nil || fake.logins != 3 {
		t.Fatalf("logged in after the limit (logins %d)", fake.logins)
	}
	restarted := newGuardedStore(t, path, fake, &now)
	if _, err := restarted.account(); err == nil || fake.logins != 3 {
		t.Fatalf("restart reset the limit (logins %d)", fake.logins)
	}

	// A manual login is allowed, and success re-enables automatic login.
	fake.loginErr = nil
	if _, _, err := restarted.StartLogin(); err != nil {
		t.Fatal(err)
	}
	if restarted.guard.Failures != 0 {
		t.Fatal("success did not reset the failure count")
	}
}

func TestStoreReportsTwoFactorOnManualLogin(t *testing.T) {
	now := time.Now()
	fake := &fakeAppStore{loginErr: appstore.ErrAuthCodeRequired}
	s := newGuardedStore(t, "", fake, &now)
	email, needsCode, err := s.StartLogin()
	if err != nil || !needsCode || email != "me@example.com" {
		t.Fatalf("StartLogin = %q %v %v", email, needsCode, err)
	}
	if s.guard.Failures != 0 {
		t.Fatal("manual attempts must not count against the automatic limit")
	}
}

func TestCheckModeNeverLogsIn(t *testing.T) {
	now := time.Now()
	guard, _ := openLoginGuard("", 0)
	fake := &fakeAppStore{account: appstore.Account{Email: "me@example.com", Password: "pw"}, signedIn: true}
	s := &Store{as: fake, guard: guard, now: func() time.Time { return now }}
	if _, err := s.relogin(fake.account); err == nil || fake.logins != 0 {
		t.Fatalf("check-mode store logged in (logins %d, err %v)", fake.logins, err)
	}
}

// Regression: an expired token reported by Purchase (while acquiring a
// license for a not-yet-owned app) must trigger a re-login, like ipatool's
// own `download --purchase`, instead of failing the request.
func TestStoreReloginsWhenPurchaseReportsExpiredToken(t *testing.T) {
	now := time.Now()
	fake := &fakeAppStore{
		signedIn: true, account: appstore.Account{Email: "me@example.com", Password: "pw", PasswordToken: "old"},
		purchaseErrs: []error{fmt.Errorf("failed to purchase item with param 'STDQ': %w", appstore.ErrPasswordTokenExpired)},
	}
	s := newGuardedStore(t, "", fake, &now)
	s.purchase = true
	owned := false
	calls := 0
	err := s.call(&appstore.App{ID: 1}, true, func(acc appstore.Account) error {
		calls++
		if !owned && fake.purchases > 0 && len(fake.purchaseErrs) == 0 && acc.PasswordToken == "token" {
			owned = true
		}
		if !owned {
			return appstore.ErrLicenseRequired
		}
		return nil
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if fake.logins != 1 || fake.purchases != 2 {
		t.Fatalf("logins %d purchases %d, want 1 and 2", fake.logins, fake.purchases)
	}
}

func TestStoreCallGivesUpAfterOneRelogin(t *testing.T) {
	now := time.Now()
	fake := &fakeAppStore{signedIn: true, account: appstore.Account{Email: "me@example.com", Password: "pw", PasswordToken: "old"}}
	s := newGuardedStore(t, "", fake, &now)
	err := s.call(&appstore.App{ID: 1}, true, func(appstore.Account) error { return appstore.ErrPasswordTokenExpired })
	if !errors.Is(err, appstore.ErrPasswordTokenExpired) || fake.logins != 1 {
		t.Fatalf("err %v logins %d; want the expired error after exactly one re-login", err, fake.logins)
	}
}
