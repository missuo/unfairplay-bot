package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Stage string

const (
	StageQueued         Stage = "queued"
	StageDownloading    Stage = "downloading"
	StagePatching       Stage = "patching"
	StageWaitingDecrypt Stage = "waiting_decrypt"
	StageDecrypting     Stage = "decrypting"
	StageFinalizing     Stage = "finalizing"
	StageReady          Stage = "ready"
	StageFailed         Stage = "failed"
	StageCanceled       Stage = "canceled"
)

func (s Stage) Terminal() bool { return s == StageReady || s == StageFailed || s == StageCanceled }

type Progress struct {
	Bytes      int64   `json:"bytes"`
	Total      int64   `json:"total"`
	Percent    float64 `json:"percent"`
	SpeedBps   int64   `json:"speed_bps"`
	ETASeconds int64   `json:"eta_seconds,omitempty"`
}

type TaskError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *TaskError) Error() string { return e.Code + ": " + e.Message }

// TaskView is the JSON shape of a task. Artifact is filled in by the HTTP
// layer because it carries a freshly signed URL.
type TaskView struct {
	ID         string        `json:"id"`
	Status     Stage         `json:"status"`
	App        Target        `json:"app"`
	Position   int           `json:"queue_position,omitempty"`
	Progress   *Progress     `json:"progress,omitempty"`
	Error      *TaskError    `json:"error,omitempty"`
	ArtifactID string        `json:"-"`
	Artifact   *ArtifactView `json:"artifact,omitempty"`
	CreatedAt  time.Time     `json:"created_at"`
	StartedAt  *time.Time    `json:"started_at,omitempty"`
	FinishedAt *time.Time    `json:"finished_at,omitempty"`
}

type task struct {
	view    TaskView
	cancel  context.CancelFunc
	changed chan struct{}
}

// Pipeline is the work a task performs; Store and Decrypter implement it in
// production and tests substitute a fake.
type Pipeline interface {
	Download(ctx context.Context, t Target, dir string, report func(Stage, *Progress)) (string, error)
	Decrypt(ctx context.Context, input, output string) error
}

var (
	ErrQueueFull = errors.New("task queue is full")
	ErrDraining  = errors.New("server is shutting down")
)

// Manager runs at most `concurrency` tasks at once; the rest wait in FIFO
// order. Decryption has its own, smaller limit because it is CPU-bound.
type Manager struct {
	mu        sync.Mutex
	cond      *sync.Cond
	tasks     map[string]*task
	active    map[string]*task // by build key, non-terminal only
	queue     []*task
	running   int
	workers   int
	draining  bool
	maxQueue  int
	retention time.Duration
	timeout   time.Duration
	workDir   string
	decrypt   chan struct{}
	pipeline  Pipeline
	cache     *Cache
	wg        sync.WaitGroup
	now       func() time.Time
}

type ManagerOptions struct {
	Concurrency, DecryptConcurrency, MaxQueue int
	Timeout, Retention                        time.Duration
	WorkDir                                   string
}

func NewManager(o ManagerOptions, p Pipeline, c *Cache) (*Manager, error) {
	// Work directories never survive a restart: their tasks are gone.
	if err := os.RemoveAll(o.WorkDir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(o.WorkDir, 0o700); err != nil {
		return nil, err
	}
	m := &Manager{
		tasks: map[string]*task{}, active: map[string]*task{},
		workers: o.Concurrency, maxQueue: o.MaxQueue, retention: o.Retention, timeout: o.Timeout, workDir: o.WorkDir,
		decrypt: make(chan struct{}, o.DecryptConcurrency), pipeline: p, cache: c, now: time.Now,
	}
	m.cond = sync.NewCond(&m.mu)
	for range o.Concurrency {
		m.wg.Add(1)
		go m.worker()
	}
	return m, nil
}

func (m *Manager) notifyLocked(t *task) {
	close(t.changed)
	t.changed = make(chan struct{})
}

func (m *Manager) reindexQueueLocked() {
	for i, t := range m.queue {
		if t.view.Position != i+1 {
			t.view.Position = i + 1
			m.notifyLocked(t)
		}
	}
}

// Submit queues a build, or returns the task already working on it.
func (m *Manager) Submit(target Target) (TaskView, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.draining {
		return TaskView{}, false, ErrDraining
	}
	if t := m.active[target.Key]; t != nil {
		return t.view, false, nil
	}
	if m.running+len(m.queue) >= m.workers+m.maxQueue {
		return TaskView{}, false, ErrQueueFull
	}
	t := &task{
		view:    TaskView{ID: newID(), Status: StageQueued, App: target, CreatedAt: m.now()},
		changed: make(chan struct{}),
	}
	m.tasks[t.view.ID] = t
	m.active[target.Key] = t
	m.queue = append(m.queue, t)
	t.view.Position = len(m.queue)
	m.cond.Signal()
	return t.view, true, nil
}

// Get returns a snapshot and a channel closed on the next change.
func (m *Manager) Get(id string) (TaskView, <-chan struct{}, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.tasks[id]
	if t == nil {
		return TaskView{}, nil, false
	}
	return t.view, t.changed, true
}

func (m *Manager) List() []TaskView {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]TaskView, 0, len(m.tasks))
	for _, t := range m.tasks {
		out = append(out, t.view)
	}
	return out
}

func (m *Manager) Cancel(id string) (TaskView, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.tasks[id]
	if t == nil {
		return TaskView{}, false
	}
	if t.view.Status == StageQueued {
		for i, q := range m.queue {
			if q == t {
				m.queue = append(m.queue[:i], m.queue[i+1:]...)
				break
			}
		}
		m.finishLocked(t, StageCanceled, &TaskError{Code: "canceled", Message: "task was canceled"})
		m.reindexQueueLocked()
	} else if t.cancel != nil {
		t.cancel()
	}
	return t.view, true
}

type Status struct {
	Running  int  `json:"running"`
	Queued   int  `json:"queued"`
	Draining bool `json:"draining"`
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Status{Running: m.running, Queued: len(m.queue), Draining: m.draining}
}

func (m *Manager) update(t *task, stage Stage, p *Progress) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t.view.Status.Terminal() || (t.view.Status == stage && p == nil) {
		return
	}
	t.view.Status, t.view.Progress = stage, p
	m.notifyLocked(t)
}

func (m *Manager) finishLocked(t *task, stage Stage, err *TaskError) {
	now := m.now()
	t.view.Status, t.view.Error, t.view.FinishedAt, t.view.Position, t.view.Progress = stage, err, &now, 0, nil
	t.cancel = nil
	if m.active[t.view.App.Key] == t {
		delete(m.active, t.view.App.Key)
	}
	m.notifyLocked(t)
}

func (m *Manager) worker() {
	defer m.wg.Done()
	for {
		m.mu.Lock()
		for len(m.queue) == 0 && !m.draining {
			m.cond.Wait()
		}
		if len(m.queue) == 0 {
			m.mu.Unlock()
			return
		}
		t := m.queue[0]
		m.queue = m.queue[1:]
		m.reindexQueueLocked()
		ctx, cancel := context.WithTimeout(context.Background(), m.timeout)
		now := m.now()
		t.cancel, t.view.StartedAt, t.view.Position, t.view.Status = cancel, &now, 0, StageDownloading
		m.running++
		m.notifyLocked(t)
		m.mu.Unlock()

		artifact, err := m.process(ctx, t)
		if err != nil && ctx.Err() != nil {
			err = ctx.Err()
		}
		cancel()

		m.mu.Lock()
		m.running--
		if err != nil {
			te := classify(err)
			stage := StageFailed
			if te.Code == "canceled" {
				stage = StageCanceled
			}
			slog.Warn("task failed", "id", t.view.ID, "key", t.view.App.Key, "code", te.Code, "err", te.Message)
			m.finishLocked(t, stage, te)
		} else {
			t.view.ArtifactID = artifact.ID
			slog.Info("task ready", "id", t.view.ID, "key", artifact.Key, "version", artifact.Version, "size", artifact.Size)
			m.finishLocked(t, StageReady, nil)
		}
		m.mu.Unlock()
	}
}

func (m *Manager) process(ctx context.Context, t *task) (Artifact, error) {
	target := t.view.App
	dir := filepath.Join(m.workDir, t.view.ID)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return Artifact{}, err
	}
	defer os.RemoveAll(dir)

	encrypted, err := m.pipeline.Download(ctx, target, dir, func(s Stage, p *Progress) { m.update(t, s, p) })
	if err != nil {
		return Artifact{}, err
	}

	m.update(t, StageWaitingDecrypt, nil)
	select {
	case m.decrypt <- struct{}{}:
	case <-ctx.Done():
		return Artifact{}, ctx.Err()
	}
	m.update(t, StageDecrypting, nil)
	decrypted := filepath.Join(dir, "decrypted.ipa")
	err = m.pipeline.Decrypt(ctx, encrypted, decrypted)
	<-m.decrypt
	if err != nil {
		return Artifact{}, err
	}
	_ = os.Remove(encrypted)

	m.update(t, StageFinalizing, nil)
	a, err := describeIPA(decrypted, target)
	if err != nil {
		return Artifact{}, err
	}
	return m.cache.Add(a, decrypted)
}

// Drain stops accepting tasks and waits for queued and running ones.
func (m *Manager) Drain(ctx context.Context) error {
	m.mu.Lock()
	m.draining = true
	m.cond.Broadcast()
	m.mu.Unlock()
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		m.mu.Lock()
		for _, t := range m.tasks {
			if t.cancel != nil {
				t.cancel()
			}
		}
		m.mu.Unlock()
		<-done
		return ctx.Err()
	}
}

// Prune forgets finished tasks older than the retention period.
func (m *Manager) Prune() {
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff := m.now().Add(-m.retention)
	for id, t := range m.tasks {
		if t.view.FinishedAt != nil && t.view.FinishedAt.Before(cutoff) {
			delete(m.tasks, id)
		}
	}
}
