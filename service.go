package main

import (
	"context"
	"log/slog"
	"time"
)

type Resolver interface {
	Resolve(ctx context.Context, bundleID string, appID int64, purchase bool) (AppInfo, error)
}

// Service is the request flow shared by the HTTP API and the Telegram bot.
type Service struct {
	resolver Resolver
	manager  *Manager
	cache    *Cache
	signer   Signer
	health   *Health
}

type ArtifactView struct {
	ID                string    `json:"id"`
	AppID             int64     `json:"app_id"`
	BundleID          string    `json:"bundle_id"`
	Name              string    `json:"name,omitempty"`
	Version           string    `json:"version,omitempty"`
	ExternalVersionID string    `json:"external_version_id"`
	FileName          string    `json:"file_name"`
	Size              int64     `json:"size"`
	SHA256            string    `json:"sha256"`
	Hits              int       `json:"hits"`
	ExpiresAt         time.Time `json:"expires_at"`
	DownloadURL       string    `json:"download_url"`
}

// Outcome of a request: either a cached artifact or a task (new or shared).
type Outcome struct {
	Cached   bool          `json:"cached"`
	Created  bool          `json:"-"`
	Artifact *ArtifactView `json:"artifact,omitempty"`
	Task     *TaskView     `json:"task,omitempty"`
}

// Resolve looks the app up. purchase allows acquiring a license for a free
// app the account does not own yet; previews pass false.
func (s *Service) Resolve(ctx context.Context, bundleID string, appID int64, purchase bool) (AppInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	info, err := s.resolver.Resolve(ctx, bundleID, appID, purchase)
	if err != nil {
		s.StoreFailed(classify(err))
	}
	return info, err
}

// StoreFailed re-checks App Store health right away after a failure that
// may mean a lost session or a changed API.
func (s *Service) StoreFailed(e *TaskError) {
	switch e.Code {
	case "auth_required", "store_error", "unavailable", "timeout":
		if s.health != nil {
			s.health.Trigger()
		}
	}
}

// IsCached reports whether the build is cached, without counting a hit.
func (s *Service) IsCached(t Target) bool { return s.cache.Has(t.Key) }

// Request serves a resolved build from cache (counting a hit) or queues it.
func (s *Service) Request(t Target) (Outcome, error) {
	if a, ok := s.cache.Hit(t.Key); ok {
		slog.Info("cache hit", "key", a.Key, "hits", a.Hits)
		return Outcome{Cached: true, Artifact: s.ArtifactView(a.ID)}, nil
	}
	view, created, err := s.manager.Submit(t)
	if err != nil {
		return Outcome{}, err
	}
	if created {
		slog.Info("task queued", "id", view.ID, "key", t.Key)
	}
	view = s.TaskView(view)
	return Outcome{Created: created, Task: &view}, nil
}

func (s *Service) ArtifactView(id string) *ArtifactView {
	a, _, ok := s.cache.Get(id, false)
	if !ok {
		return nil
	}
	return &ArtifactView{
		ID: a.ID, AppID: a.AppID, BundleID: a.BundleID, Name: a.Name, Version: a.Version,
		ExternalVersionID: a.ExternalVersionID, FileName: a.FileName, Size: a.Size, SHA256: a.SHA256,
		Hits: a.Hits, ExpiresAt: a.ExpiresAt,
		// The link lives exactly as long as the artifact is guaranteed to.
		DownloadURL: s.signer.URL(a.ID, a.FileName, a.ExpiresAt),
	}
}

func (s *Service) TaskView(v TaskView) TaskView {
	if v.ArtifactID != "" {
		v.Artifact = s.ArtifactView(v.ArtifactID)
	}
	return v
}
