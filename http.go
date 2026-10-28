package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

type Server struct {
	*Service
	token    string
	versions map[string]string
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /d/{id}/{name}", s.download)
	mux.HandleFunc("HEAD /d/{id}/{name}", s.download)
	mux.Handle("POST /v1/tasks", s.auth(s.createTask))
	mux.Handle("GET /v1/tasks", s.auth(s.listTasks))
	mux.Handle("GET /v1/tasks/{id}", s.auth(s.getTask))
	mux.Handle("DELETE /v1/tasks/{id}", s.auth(s.cancelTask))
	mux.Handle("GET /v1/tasks/{id}/events", s.auth(s.taskEvents))
	mux.Handle("GET /v1/status", s.auth(s.status))
	return mux
}

// auth accepts a bearer token, or ?token= for clients such as EventSource
// that cannot set headers.
func (s *Server) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			got = r.URL.Query().Get("token")
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			writeError(w, http.StatusUnauthorized, &TaskError{Code: "unauthorized", Message: "missing or invalid API token"})
			return
		}
		next(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, e *TaskError) {
	writeJSON(w, status, map[string]any{"error": e})
}

var errorStatus = map[string]int{
	"bad_request":           http.StatusBadRequest,
	"not_found":             http.StatusNotFound,
	"license_required":      http.StatusForbidden,
	"subscription_required": http.StatusForbidden,
	"auth_required":         http.StatusServiceUnavailable,
	"unavailable":           http.StatusServiceUnavailable,
	"timeout":               http.StatusGatewayTimeout,
}

var bundleIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.\-_]{0,254}$`)

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BundleID string `json:"bundle_id"`
		AppID    int64  `json:"app_id"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, &TaskError{Code: "bad_request", Message: "invalid JSON body: " + err.Error()})
		return
	}
	if req.BundleID == "" && req.AppID == 0 {
		writeError(w, http.StatusBadRequest, &TaskError{Code: "bad_request", Message: "bundle_id or app_id is required"})
		return
	}
	if (req.BundleID != "" && !bundleIDPattern.MatchString(req.BundleID)) || req.AppID < 0 {
		writeError(w, http.StatusBadRequest, &TaskError{Code: "bad_request", Message: "invalid bundle_id or app_id"})
		return
	}

	// Always ask the App Store for the current build: a cached artifact only
	// counts as a hit if it is that exact build.
	info, err := s.Resolve(r.Context(), req.BundleID, req.AppID)
	if err != nil {
		writeError(w, statusFor(classify(err)), classify(err))
		return
	}
	out, err := s.Request(info.Target)
	if err != nil {
		writeRequestError(w, err)
		return
	}
	status := http.StatusOK
	if out.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, out)
}

func statusFor(e *TaskError) int {
	if status, ok := errorStatus[e.Code]; ok {
		return status
	}
	return http.StatusBadGateway
}

func writeRequestError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrQueueFull):
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, &TaskError{Code: "queue_full", Message: err.Error()})
	case errors.Is(err, ErrDraining):
		w.Header().Set("Retry-After", "120")
		writeError(w, http.StatusServiceUnavailable, &TaskError{Code: "draining", Message: err.Error()})
	default:
		writeError(w, http.StatusInternalServerError, &TaskError{Code: "internal", Message: err.Error()})
	}
}

func (s *Server) listTasks(w http.ResponseWriter, _ *http.Request) {
	views := s.manager.List()
	sort.Slice(views, func(i, j int) bool { return views[i].CreatedAt.After(views[j].CreatedAt) })
	for i := range views {
		views[i] = s.TaskView(views[i])
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": views})
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	v, _, ok := s.manager.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, &TaskError{Code: "not_found", Message: "no such task"})
		return
	}
	writeJSON(w, http.StatusOK, s.TaskView(v))
}

func (s *Server) cancelTask(w http.ResponseWriter, r *http.Request) {
	v, ok := s.manager.Cancel(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, &TaskError{Code: "not_found", Message: "no such task"})
		return
	}
	writeJSON(w, http.StatusAccepted, s.TaskView(v))
}

// taskEvents streams the task as Server-Sent Events: one "task" event with
// the full snapshot on every change, ending after a terminal state.
func (s *Server) taskEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, _, ok := s.manager.Get(id); !ok {
		writeError(w, http.StatusNotFound, &TaskError{Code: "not_found", Message: "no such task"})
		return
	}
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		v, changed, ok := s.manager.Get(id)
		if !ok {
			return
		}
		data, _ := json.Marshal(s.TaskView(v))
		if _, err := fmt.Fprintf(w, "event: task\ndata: %s\n\n", data); err != nil {
			return
		}
		_ = rc.Flush()
		if v.Status.Terminal() {
			return
		}
		for waiting := true; waiting; {
			select {
			case <-changed:
				waiting = false
			case <-r.Context().Done():
				return
			case <-heartbeat.C:
				if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
					return
				}
				_ = rc.Flush()
			}
		}
	}
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"tasks": s.manager.Status(), "cache": s.cache.Stats(), "versions": s.versions, "health": s.health.State(),
	})
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	q := r.URL.Query()
	if !artifactIDPattern.MatchString(id) || !s.signer.Verify(id, q.Get("exp"), q.Get("sig"), time.Now()) {
		http.Error(w, "invalid or expired link", http.StatusForbidden)
		return
	}
	a, path, ok := s.cache.Get(id, r.Method == http.MethodGet && r.Header.Get("Range") == "")
	if !ok {
		http.Error(w, "file expired", http.StatusGone)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "file expired", http.StatusGone)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, a.FileName))
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-SHA256", a.SHA256)
	http.ServeContent(w, r, a.FileName, a.CreatedAt, f)
}
