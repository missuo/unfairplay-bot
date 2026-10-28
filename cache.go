package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Artifact is a cached decrypted IPA. Its key identifies one exact App Store
// build: trackId plus the external version identifier.
type Artifact struct {
	ID                string    `json:"id"`
	Key               string    `json:"key"`
	AppID             int64     `json:"app_id"`
	BundleID          string    `json:"bundle_id"`
	Name              string    `json:"name"`
	Version           string    `json:"version"`
	ExternalVersionID string    `json:"external_version_id"`
	FileName          string    `json:"file_name"`
	Size              int64     `json:"size"`
	SHA256            string    `json:"sha256"`
	CreatedAt         time.Time `json:"created_at"`
	LastHit           time.Time `json:"last_hit"`
	ExpiresAt         time.Time `json:"expires_at"`
	Hits              int       `json:"hits"`
}

var artifactIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Cache keeps artifacts on disk with a sliding expiry: every hit pushes the
// expiry to at least now+ttl, so popular builds stay until they go a full TTL
// without a hit. A size cap, when set, evicts the least recently hit first.
type Cache struct {
	mu       sync.Mutex
	dir      string
	ttl      time.Duration
	maxBytes int64
	items    map[string]*Artifact
	keys     map[string]string
	now      func() time.Time
}

func OpenCache(dir string, ttl time.Duration, maxBytes int64) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	c := &Cache{dir: dir, ttl: ttl, maxBytes: maxBytes, items: map[string]*Artifact{}, keys: map[string]string{}, now: time.Now}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		name := entry.Name()
		id, isMeta := strings.CutSuffix(name, ".json")
		if !isMeta {
			if ipa, ok := strings.CutSuffix(name, ".ipa"); !ok || !artifactIDPattern.MatchString(ipa) {
				_ = os.RemoveAll(filepath.Join(dir, name)) // leftover temp files
			}
			continue
		}
		var a Artifact
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err == nil {
			err = json.Unmarshal(data, &a)
		}
		if err != nil || a.ID != id || !artifactIDPattern.MatchString(id) {
			slog.Warn("dropping unreadable artifact metadata", "file", name, "err", err)
			_ = os.Remove(filepath.Join(dir, name))
			continue
		}
		if info, err := os.Stat(c.path(id)); err != nil || info.Size() != a.Size {
			c.removeFiles(id)
			continue
		}
		c.items[id] = &a
		c.keys[a.Key] = id
	}
	// Remove IPA files whose metadata is missing.
	for _, entry := range entries {
		if id, ok := strings.CutSuffix(entry.Name(), ".ipa"); ok && c.items[id] == nil {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
	c.Sweep()
	return c, nil
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (c *Cache) path(id string) string     { return filepath.Join(c.dir, id+".ipa") }
func (c *Cache) metaPath(id string) string { return filepath.Join(c.dir, id+".json") }

func (c *Cache) removeFiles(id string) {
	_ = os.Remove(c.path(id))
	_ = os.Remove(c.metaPath(id))
}

func (c *Cache) persist(a *Artifact) error {
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.metaPath(a.ID) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.metaPath(a.ID))
}

func (c *Cache) touchLocked(a *Artifact) {
	now := c.now()
	a.LastHit = now
	a.Hits++
	if exp := now.Add(c.ttl); exp.After(a.ExpiresAt) {
		a.ExpiresAt = exp
	}
	if err := c.persist(a); err != nil {
		slog.Warn("persist artifact", "id", a.ID, "err", err)
	}
}

// Hit returns the live artifact for key and extends its expiry.
func (c *Cache) Hit(key string) (Artifact, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.liveLocked(c.keys[key])
	if a == nil {
		return Artifact{}, false
	}
	c.touchLocked(a)
	return *a, true
}

// Has reports whether key has a live artifact without counting a hit.
func (c *Cache) Has(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.liveLocked(c.keys[key]) != nil
}

// Get returns a live artifact by ID. With touch, the access counts as a hit.
func (c *Cache) Get(id string, touch bool) (Artifact, string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.liveLocked(id)
	if a == nil {
		return Artifact{}, "", false
	}
	if touch {
		c.touchLocked(a)
	}
	return *a, c.path(id), true
}

func (c *Cache) liveLocked(id string) *Artifact {
	a := c.items[id]
	if a == nil || !c.now().Before(a.ExpiresAt) {
		return nil
	}
	return a
}

// Add moves a finished IPA into the cache.
func (c *Cache) Add(a Artifact, src string) (Artifact, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	a.ID, a.CreatedAt, a.LastHit, a.ExpiresAt, a.Hits = newID(), now, now, now.Add(c.ttl), 0
	if err := os.Rename(src, c.path(a.ID)); err != nil {
		return Artifact{}, fmt.Errorf("move artifact into cache: %w", err)
	}
	if err := c.persist(&a); err != nil {
		c.removeFiles(a.ID)
		return Artifact{}, err
	}
	if old := c.keys[a.Key]; old != "" {
		c.deleteLocked(old)
	}
	c.items[a.ID] = &a
	c.keys[a.Key] = a.ID
	c.evictLocked(a.ID)
	return a, nil
}

func (c *Cache) deleteLocked(id string) {
	if a := c.items[id]; a != nil {
		if c.keys[a.Key] == id {
			delete(c.keys, a.Key)
		}
		delete(c.items, id)
	}
	c.removeFiles(id)
}

func (c *Cache) evictLocked(keep string) {
	if c.maxBytes <= 0 {
		return
	}
	var total int64
	list := make([]*Artifact, 0, len(c.items))
	for _, a := range c.items {
		total += a.Size
		if a.ID != keep {
			list = append(list, a)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].LastHit.Before(list[j].LastHit) })
	for _, a := range list {
		if total <= c.maxBytes {
			return
		}
		slog.Info("evicting artifact for size cap", "id", a.ID, "key", a.Key, "size", a.Size)
		total -= a.Size
		c.deleteLocked(a.ID)
	}
}

// Sweep deletes expired artifacts.
func (c *Cache) Sweep() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for id, a := range c.items {
		if !now.Before(a.ExpiresAt) {
			slog.Info("expiring artifact", "id", id, "key", a.Key, "hits", a.Hits)
			c.deleteLocked(id)
		}
	}
}

type CacheStats struct {
	Artifacts int   `json:"artifacts"`
	Bytes     int64 `json:"bytes"`
	MaxBytes  int64 `json:"max_bytes"`
}

func (c *Cache) Stats() CacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := CacheStats{Artifacts: len(c.items), MaxBytes: c.maxBytes}
	for _, a := range c.items {
		s.Bytes += a.Size
	}
	return s
}
