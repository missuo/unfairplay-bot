package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/schollz/progressbar/v3"
)

// fakePipeline blocks each download until released, so tests control timing.
type fakePipeline struct {
	mu       sync.Mutex
	running  int
	peak     int
	started  chan string
	release  map[string]chan struct{}
	failWith map[string]error
}

func newFakePipeline() *fakePipeline {
	return &fakePipeline{started: make(chan string, 100), release: map[string]chan struct{}{}, failWith: map[string]error{}}
}

func (f *fakePipeline) gate(key string) chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.release[key] == nil {
		f.release[key] = make(chan struct{})
	}
	return f.release[key]
}

func (f *fakePipeline) Download(ctx context.Context, t Target, dir string, report func(Stage, *Progress)) (string, error) {
	f.mu.Lock()
	f.running++
	f.peak = max(f.peak, f.running)
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.running--; f.mu.Unlock() }()
	f.started <- t.Key
	report(StageDownloading, &Progress{Bytes: 5, Total: 10, Percent: 50})
	select {
	case <-f.gate(t.Key):
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if err := f.failWith[t.Key]; err != nil {
		return "", err
	}
	path := filepath.Join(dir, "encrypted.ipa")
	return path, writeIPA(path, t)
}

func (f *fakePipeline) Decrypt(_ context.Context, input, output string) error {
	return os.Rename(input, output)
}

func writeIPA(path string, t Target) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	zw := zip.NewWriter(file)
	w, _ := zw.Create("iTunesMetadata.plist")
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>apple-id</key><string>me@example.com</string>
<key>userName</key><string>me@example.com</string>
<key>itemName</key><string>Example</string>
<key>bundleShortVersionString</key><string>8.0.%s</string>
<key>softwareVersionExternalIdentifier</key><integer>%s</integer>
<key>softwareVersionBundleId</key><string>com.example.app</string>
<key>com.apple.iTunesStore.downloadInfo</key><dict>
  <key>accountInfo</key><dict><key>AppleID</key><string>me@example.com</string><key>DSPersonID</key><integer>123</integer><key>FirstName</key><string>Me</string></dict>
  <key>purchaseDate</key><string>2026-10-01T00:00:00Z</string>
</dict>
</dict></plist>`, t.ExternalVersionID, t.ExternalVersionID)
	for _, name := range []string{
		"Payload/Example.app/Example",
		"Payload/Example.app/SC_Info/Example.sinf",
		"Payload/Example.app/SC_Info/Example.supf",
		"Payload/Example.app/PlugIns/Widget.appex/SC_Info/Widget.sinf",
		"Payload/Example.app/Watch/W.app/sc_info/W.sinf",
	} {
		w, _ := zw.Create(name)
		w.Write([]byte(name))
	}
	return zw.Close()
}

func target(id int64, ext string) Target {
	return Target{AppID: id, ExternalVersionID: ext, Key: fmt.Sprintf("%d:%s", id, ext)}
}

func newTestManager(t *testing.T, p Pipeline, concurrency, maxQueue int) (*Manager, *Cache) {
	t.Helper()
	dir := t.TempDir()
	cache, err := OpenCache(filepath.Join(dir, "files"), time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(ManagerOptions{
		Concurrency: concurrency, DecryptConcurrency: 1, MaxQueue: maxQueue,
		Timeout: time.Minute, Retention: time.Hour, WorkDir: filepath.Join(dir, "work"),
	}, p, cache)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Drain(context.Background()) })
	return m, cache
}

func waitFor(t *testing.T, m *Manager, id string, want Stage) TaskView {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		v, changed, _ := m.Get(id)
		if v.Status == want {
			return v
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("task %s: status %s, want %s", id, v.Status, want)
		}
	}
}

func TestManagerRunsThreeAndQueuesTheRestInOrder(t *testing.T) {
	p := newFakePipeline()
	m, cache := newTestManager(t, p, 3, 100)
	var ids []string
	for i := range 5 {
		v, created, err := m.Submit(target(int64(i+1), "1"))
		if err != nil || !created {
			t.Fatalf("submit %d: created=%v err=%v", i, created, err)
		}
		ids = append(ids, v.ID)
	}
	for range 3 {
		<-p.started
	}
	if v, _, _ := m.Get(ids[3]); v.Status != StageQueued || v.Position != 1 {
		t.Fatalf("4th task: %s position %d, want queued at 1", v.Status, v.Position)
	}
	if v, _, _ := m.Get(ids[4]); v.Position != 2 {
		t.Fatalf("5th task position %d, want 2", v.Position)
	}
	if s := m.Status(); s.Running != 3 || s.Queued != 2 {
		t.Fatalf("status %+v", s)
	}

	close(p.gate("1:1"))
	waitFor(t, m, ids[0], StageReady)
	if key := <-p.started; key != "4:1" {
		t.Fatalf("next started %s, want FIFO 4:1", key)
	}
	if v, _, _ := m.Get(ids[4]); v.Position != 1 {
		t.Fatalf("5th task position %d after dequeue, want 1", v.Position)
	}
	for _, k := range []string{"2:1", "3:1", "4:1", "5:1"} {
		close(p.gate(k))
	}
	for _, id := range ids {
		waitFor(t, m, id, StageReady)
	}
	if p.peak != 3 {
		t.Fatalf("peak concurrency %d, want 3", p.peak)
	}
	if !cache.Has("5:1") {
		t.Fatal("artifact not cached")
	}
}

func TestManagerDeduplicatesActiveBuild(t *testing.T) {
	p := newFakePipeline()
	m, _ := newTestManager(t, p, 3, 100)
	a, created, _ := m.Submit(target(1, "100"))
	b, again, _ := m.Submit(target(1, "100"))
	if !created || again || a.ID != b.ID {
		t.Fatalf("duplicate submit: created=%v again=%v ids %s %s", created, again, a.ID, b.ID)
	}
	// A newer build of the same app is different work.
	c, created, _ := m.Submit(target(1, "101"))
	if !created || c.ID == a.ID {
		t.Fatal("new build should start a new task")
	}
	close(p.gate("1:100"))
	close(p.gate("1:101"))
	waitFor(t, m, a.ID, StageReady)
	waitFor(t, m, c.ID, StageReady)
}

func TestManagerQueueLimit(t *testing.T) {
	p := newFakePipeline()
	m, _ := newTestManager(t, p, 1, 1)
	m.Submit(target(1, "1"))
	<-p.started
	if _, _, err := m.Submit(target(2, "1")); err != nil {
		t.Fatalf("queued submit: %v", err)
	}
	if _, _, err := m.Submit(target(3, "1")); err != ErrQueueFull {
		t.Fatalf("got %v, want ErrQueueFull", err)
	}
	close(p.gate("1:1"))
	close(p.gate("2:1"))
}

func TestManagerCancelQueuedAndRunning(t *testing.T) {
	p := newFakePipeline()
	m, _ := newTestManager(t, p, 1, 10)
	running, _, _ := m.Submit(target(1, "1"))
	queued, _, _ := m.Submit(target(2, "1"))
	<-p.started
	m.Cancel(queued.ID)
	waitFor(t, m, queued.ID, StageCanceled)
	m.Cancel(running.ID)
	v := waitFor(t, m, running.ID, StageCanceled)
	if v.Error == nil || v.Error.Code != "canceled" {
		t.Fatalf("error %+v", v.Error)
	}
	// The canceled build can be submitted again.
	if _, created, _ := m.Submit(target(1, "1")); !created {
		t.Fatal("resubmit after cancel should create a task")
	}
	close(p.gate("1:1"))
}

func TestManagerFailureReleasesSlot(t *testing.T) {
	p := newFakePipeline()
	p.failWith["1:1"] = &TaskError{Code: "license_required", Message: "x"}
	m, _ := newTestManager(t, p, 1, 10)
	v, _, _ := m.Submit(target(1, "1"))
	close(p.gate("1:1"))
	v = waitFor(t, m, v.ID, StageFailed)
	if v.Error.Code != "license_required" {
		t.Fatalf("code %s", v.Error.Code)
	}
	w, _, _ := m.Submit(target(2, "1"))
	close(p.gate("2:1"))
	waitFor(t, m, w.ID, StageReady)
}

func TestDescribeIPARejectsDifferentBuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.ipa")
	if err := writeIPA(path, target(1, "200")); err != nil {
		t.Fatal(err)
	}
	if _, err := describeIPA(path, target(1, "201")); classify(err).Code != "version_mismatch" {
		t.Fatalf("got %v", err)
	}
	a, err := describeIPA(path, target(1, "200"))
	if err != nil || a.Version != "8.0.200" || a.FileName != "com.example.app_8.0.200.decrypted.ipa" {
		t.Fatalf("%+v %v", a, err)
	}
}

func TestCacheSlidingExpiry(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c, _ := OpenCache(dir, 24*time.Hour, 0)
	c.now = func() time.Time { return now }
	src := filepath.Join(dir, "src")
	os.WriteFile(src, []byte("ipa"), 0o600)
	a, err := c.Add(Artifact{Key: "1:1", Size: 3}, src)
	if err != nil {
		t.Fatal(err)
	}

	now = now.Add(20 * time.Hour)
	if _, ok := c.Hit("1:1"); !ok {
		t.Fatal("hit within TTL")
	}
	now = now.Add(20 * time.Hour) // 40h after creation, 20h after last hit
	c.Sweep()
	if !c.Has("1:1") {
		t.Fatal("hit should have extended expiry")
	}
	now = now.Add(4 * time.Hour) // 24h after last hit
	c.Sweep()
	if c.Has("1:1") {
		t.Fatal("artifact should expire 24h after last hit")
	}
	if _, err := os.Stat(c.path(a.ID)); !os.IsNotExist(err) {
		t.Fatal("file should be deleted")
	}
}

func TestCacheReloadAndSizeCap(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	c, _ := OpenCache(dir, time.Hour, 10)
	c.now = func() time.Time { return now }
	add := func(key string) Artifact {
		src := filepath.Join(dir, "src")
		os.WriteFile(src, []byte("123456"), 0o600)
		a, err := c.Add(Artifact{Key: key, Size: 6}, src)
		if err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Second)
		return a
	}
	add("1:1")
	add("2:1")
	if c.Has("1:1") || !c.Has("2:1") {
		t.Fatal("size cap should evict the least recently hit artifact")
	}
	reopened, err := OpenCache(dir, time.Hour, 10)
	if err != nil || !reopened.Has("2:1") || reopened.Stats().Artifacts != 1 {
		t.Fatalf("reload: %v %+v", err, reopened.Stats())
	}
}

func TestSigner(t *testing.T) {
	s := Signer{key: []byte(strings.Repeat("k", 32)), base: "https://x"}
	now := time.Now()
	link := s.URL("abc", "a b.ipa", now.Add(time.Hour))
	if !strings.HasPrefix(link, "https://x/d/abc/a%20b.ipa?") {
		t.Fatal(link)
	}
	q := strings.SplitN(link, "?", 2)[1]
	vals := map[string]string{}
	for _, kv := range strings.Split(q, "&") {
		k, v, _ := strings.Cut(kv, "=")
		vals[k] = v
	}
	if !s.Verify("abc", vals["exp"], vals["sig"], now) {
		t.Fatal("valid link rejected")
	}
	if s.Verify("abd", vals["exp"], vals["sig"], now) || s.Verify("abc", vals["exp"], vals["sig"], now.Add(2*time.Hour)) {
		t.Fatal("wrong id or expired link accepted")
	}
}

// fakeResolver serves build ext. With unowned set, the account lacks the
// app until a Resolve with purchase acquires it.
type fakeResolver struct {
	mu        sync.Mutex
	ext       string
	unowned   bool
	purchases []bool // the purchase flag of every call
}

func (f *fakeResolver) Resolve(_ context.Context, _ string, appID int64, purchase bool) (AppInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.purchases = append(f.purchases, purchase)
	if f.unowned && !purchase {
		return AppInfo{Target: Target{AppID: appID, Name: "Example", Version: "8.0.lookup"}, NeedsLicense: true}, nil
	}
	f.unowned = false
	t := target(appID, f.ext)
	t.Name, t.Version = "Example", "8.0."+f.ext
	return AppInfo{Target: t}, nil
}

func TestHTTPFlowCacheHitOnlyForCurrentBuild(t *testing.T) {
	p := newFakePipeline()
	m, cache := newTestManager(t, p, 3, 10)
	res := &fakeResolver{ext: "79"}
	srv := &Server{Service: &Service{resolver: res, manager: m, cache: cache}, token: strings.Repeat("t", 16)}
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()
	srv.signer = Signer{key: []byte(strings.Repeat("k", 32)), base: ts.URL}

	post := func() (int, map[string]any) {
		req, _ := http.NewRequest("POST", ts.URL+"/v1/tasks", strings.NewReader(`{"app_id": 12345}`))
		req.Header.Set("Authorization", "Bearer "+srv.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body map[string]any
		json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body
	}

	code, body := post()
	if code != http.StatusCreated {
		t.Fatalf("first post %d %v", code, body)
	}
	id := body["task"].(map[string]any)["id"].(string)

	// Stream events until ready.
	req, _ := http.NewRequest("GET", ts.URL+"/v1/tasks/"+id+"/events?token="+srv.token, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	<-p.started
	close(p.gate("12345:79"))
	events, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(events), `"status":"downloading"`) || !strings.Contains(string(events), `"status":"ready"`) {
		t.Fatalf("events:\n%s", events)
	}

	code, body = post()
	if code != http.StatusOK || body["cached"] != true {
		t.Fatalf("same build should hit cache: %d %v", code, body)
	}
	link := body["artifact"].(map[string]any)["download_url"].(string)
	dl, err := http.Get(link)
	if err != nil || dl.StatusCode != http.StatusOK {
		t.Fatalf("download: %v %d", err, dl.StatusCode)
	}
	served, _ := io.ReadAll(dl.Body)
	dl.Body.Close()
	// What users download carries no licenses and no Apple ID.
	zr, err := zip.NewReader(bytes.NewReader(served), int64(len(served)))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		if isLicensePath(f.Name) {
			t.Fatalf("served IPA contains %s", f.Name)
		}
		if f.Name == "iTunesMetadata.plist" {
			rc, _ := f.Open()
			meta, _ := io.ReadAll(rc)
			rc.Close()
			if bytes.Contains(meta, []byte("me@example.com")) {
				t.Fatal("served IPA contains the Apple ID")
			}
		}
	}
	if bad, _ := http.Get(strings.Replace(link, "sig=", "sig=x", 1)); bad.StatusCode != http.StatusForbidden {
		t.Fatalf("tampered link status %d", bad.StatusCode)
	}

	// The App Store now serves a newer build: no hit, new task.
	res.ext = "80"
	code, body = post()
	if code != http.StatusCreated || body["cached"] != false {
		t.Fatalf("new build should not hit cache: %d %v", code, body)
	}
	close(p.gate("12345:80"))

	req, _ = http.NewRequest("POST", ts.URL+"/v1/tasks", strings.NewReader(`{"app_id": 1}`))
	if r, _ := http.DefaultClient.Do(req); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status %d", r.StatusCode)
	}
}

// Uses the same bar configuration as Store.Download.
func TestWatchProgressReadsBarState(t *testing.T) {
	bar := progressbar.NewOptions64(1, progressbar.OptionSetWriter(io.Discard), progressbar.OptionThrottle(time.Second))
	bar.ChangeMax64(1000)
	if _, err := bar.Write(make([]byte, 250)); err != nil {
		t.Fatal(err)
	}
	got := make(chan Progress, 1)
	done := make(chan struct{})
	defer close(done)
	go watchProgress(bar, done, func(s Stage, p *Progress) {
		if s == StageDownloading && p != nil {
			select {
			case got <- *p:
			default:
			}
		}
	})
	select {
	case p := <-got:
		if p.Bytes != 250 || p.Total != 1000 || p.Percent != 25 {
			t.Fatalf("progress %+v", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no progress reported")
	}
}
