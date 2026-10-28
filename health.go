package main

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// HealthState describes the App Store side: the ipatool session and the
// download API. UnfairPlay works offline and is not part of it.
type HealthState struct {
	Enabled   bool       `json:"enabled"`
	OK        bool       `json:"ok"`
	CheckedAt time.Time  `json:"checked_at,omitzero"`
	Since     time.Time  `json:"since,omitzero"` // when OK last changed
	Error     *TaskError `json:"error,omitempty"`
}

// Health periodically runs a real App Store request for a known app, so a
// lost session or a changed API is noticed before users hit it. Failures seen
// elsewhere call Trigger to re-check at once. onChange fires only when the
// result flips (and on the first failure).
type Health struct {
	mu       sync.Mutex
	state    HealthState
	check    func(context.Context) error
	interval time.Duration
	minGap   time.Duration
	trigger  chan struct{}
	onChange func(prev, cur HealthState)
	now      func() time.Time
}

func NewHealth(check func(context.Context) error, interval time.Duration) *Health {
	return &Health{
		state: HealthState{Enabled: check != nil, OK: true}, check: check, interval: interval,
		minGap: time.Minute, trigger: make(chan struct{}, 1), now: time.Now,
	}
}

func (h *Health) State() HealthState {
	if h == nil {
		return HealthState{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.state
}

// Trigger requests a check soon; repeated calls collapse into one.
func (h *Health) Trigger() {
	if h == nil || h.check == nil {
		return
	}
	select {
	case h.trigger <- struct{}{}:
	default:
	}
}

func (h *Health) Run(ctx context.Context) {
	if h.check == nil {
		slog.Warn("health check disabled: set UNFAIRPLAY_BOT_HEALTH_APP_ID")
		return
	}
	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()
	var last time.Time
	for {
		last = h.now()
		h.CheckNow(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-h.trigger:
			// Avoid a storm of checks when many tasks fail together.
			if wait := h.minGap - h.now().Sub(last); wait > 0 {
				select {
				case <-time.After(wait):
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

func (h *Health) CheckNow(ctx context.Context) HealthState {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	err := h.check(cctx)
	cancel()
	if ctx.Err() != nil {
		return h.State()
	}

	h.mu.Lock()
	prev := h.state
	cur := prev
	cur.CheckedAt, cur.OK, cur.Error = h.now(), err == nil, nil
	if err != nil {
		cur.Error = classify(err)
	}
	if cur.OK != prev.OK || prev.Since.IsZero() {
		cur.Since = cur.CheckedAt
	}
	h.state = cur
	onChange := h.onChange
	h.mu.Unlock()

	if err != nil {
		slog.Warn("health check failed", "code", cur.Error.Code, "err", cur.Error.Message)
	}
	changed := cur.OK != prev.OK || (!cur.OK && prev.CheckedAt.IsZero())
	if changed && onChange != nil {
		onChange(prev, cur)
	}
	return cur
}

// SetOnChange installs the change callback; call before Run.
func (h *Health) SetOnChange(fn func(prev, cur HealthState)) {
	h.mu.Lock()
	h.onChange = fn
	h.mu.Unlock()
}
