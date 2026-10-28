package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"
)

// autoLoginBackoff is the minimum gap between automatic login attempts.
const autoLoginBackoff = 15 * time.Minute

// loginGuard limits automatic Apple ID logins. Retrying a login that keeps
// failing (wrong password, or a login flow Apple no longer accepts) can get
// the account locked, so after maxFailures consecutive failures automatic
// login stops until a manual login succeeds. The count is persisted so a
// crash-restart loop cannot reset it. The caller serializes access.
type loginGuard struct {
	path        string
	maxFailures int
	Failures    int       `json:"failures"`
	LastFailure time.Time `json:"last_failure"`
}

func openLoginGuard(path string, maxFailures int) (*loginGuard, error) {
	g := &loginGuard{path: path, maxFailures: maxFailures}
	if path == "" {
		return g, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return g, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, g); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return g, nil
}

func (g *loginGuard) save() {
	if g.path == "" {
		return
	}
	data, _ := json.Marshal(g)
	tmp := g.path + ".tmp"
	err := os.WriteFile(tmp, data, 0o600)
	if err == nil {
		err = os.Rename(tmp, g.path)
	}
	if err != nil {
		slog.Error("persist automatic-login state", "err", err)
	}
}

// allow reports why an automatic attempt may not run now, if it may not.
func (g *loginGuard) allow(now time.Time) error {
	if g.Failures >= g.maxFailures {
		return &TaskError{Code: "auth_required", Message: fmt.Sprintf(
			"automatic login disabled after %d consecutive failures; check the Apple ID password, then send /login", g.Failures)}
	}
	if g.Failures > 0 && now.Sub(g.LastFailure) < autoLoginBackoff {
		return &TaskError{Code: "auth_required", Message: fmt.Sprintf(
			"automatic login failed %d/%d times; next attempt after %s", g.Failures, g.maxFailures, g.LastFailure.Add(autoLoginBackoff).Local().Format("15:04"))}
	}
	return nil
}

func (g *loginGuard) failed(now time.Time) {
	g.Failures++
	g.LastFailure = now
	g.save()
	slog.Warn("automatic Apple ID login failed", "failures", g.Failures, "max", g.maxFailures)
}

func (g *loginGuard) succeeded() {
	if g.Failures != 0 {
		g.Failures, g.LastFailure = 0, time.Time{}
		g.save()
	}
}
