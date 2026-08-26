// Package api is ClipHanger's client-facing HTTP surface — exactly
// what docs/API.md describes, nothing more. Every handler here assumes
// requireAPIKey has already run; package web (the credential-owning
// settings UI) is deliberately separate and never mounted under this
// package's auth.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/sreenathkc/cliphanger/internal/extract"
	"github.com/sreenathkc/cliphanger/internal/model"
	"github.com/sreenathkc/cliphanger/internal/queue"
	"github.com/sreenathkc/cliphanger/internal/store"
)

const version = "0.1.0"

type Server struct {
	store  *store.Store
	queue  *queue.Queue
	mux    *http.ServeMux
	logger *slog.Logger
}

func New(st *store.Store, q *queue.Queue, logger *slog.Logger) *Server {
	s := &Server{store: st, queue: q, mux: http.NewServeMux(), logger: logger}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// statusRecorder/withLogging mirror package web's own (2026-08-23,
// "nothing happened in the browser with no way to tell, server-side,
// whether the request even arrived") — the exact same class of gap,
// just never applied here. This package is the actual client-facing
// surface (what DemoFlex or any other client talks to), and until now
// it logged NOTHING per request — a real report (2026-08-26: "Force
// Refresh isn't adding any jobs... I think it should say the fail
// reason either at client side or at the cliphanger log") traced back
// to exactly this: the client's job WAS rejected with a clear 400
// ("references unknown serverId") and that reason WAS sent back in the
// response body, but there was no server-side trace of the request
// having happened at all, making it much harder to diagnose from the
// server side than it needed to be. Wrapping every route here the same
// way package web already does closes that gap for the surface that
// actually matters most — this is what every real client talks to.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// withLogging wraps OUTSIDE requireAPIKey, same reasoning as web's own
// version — a request rejected for a bad/missing API key still gets
// logged, not just successfully authenticated ones.
func (s *Server) withLogging(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			s.logger.Info("api request", "method", r.Method, "path", r.URL.Path, "status", rec.status, "duration", time.Since(start))
		}()
		next(rec, r)
	}
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /health", s.withLogging(s.requireAPIKey(s.handleHealth)))
	s.mux.HandleFunc("GET /servers", s.withLogging(s.requireAPIKey(s.handleListServers)))
	s.mux.HandleFunc("POST /jobs", s.withLogging(s.requireAPIKey(s.handleSubmitJobs)))
	s.mux.HandleFunc("GET /jobs", s.withLogging(s.requireAPIKey(s.handleListJobs)))
	s.mux.HandleFunc("GET /jobs/{captureId}", s.withLogging(s.requireAPIKey(s.handleGetJob)))
	s.mux.HandleFunc("DELETE /jobs/{captureId}", s.withLogging(s.requireAPIKey(s.handleDeleteJob)))
	s.mux.HandleFunc("GET /media/{captureId}/still", s.withLogging(s.requireAPIKey(s.handleMedia(false))))
	s.mux.HandleFunc("GET /media/{captureId}/clip", s.withLogging(s.requireAPIKey(s.handleMedia(true))))
}

// requireAPIKey rejects anything without a matching X-Api-Key header —
// "anything without it gets 401 and no body" (docs/API.md). Constant-
// time comparison so response timing can't be used to guess the key
// byte by byte.
func (s *Server) requireAPIKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		want, err := s.store.APIKey()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		got := r.Header.Get("X-Api-Key")
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// --- GET /health ---

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	queued, running, failed := s.store.Counts()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"service": "cliphanger",
		"version": version,
		"ffmpeg":  extract.Version(r.Context()),
		"servers": len(s.store.ListServers()),
		"queued":  queued,
		"running": running,
		"failed":  failed,
	})
}

// --- GET /servers ---

func (s *Server) handleListServers(w http.ResponseWriter, r *http.Request) {
	servers := s.store.ListServers()
	public := make([]model.PublicServer, 0, len(servers))
	for _, srv := range servers {
		public = append(public, srv.Public())
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"servers": public})
}

// --- POST /jobs ---

type submitJobRequest struct {
	Force bool `json:"force"`
	Jobs  []struct {
		CaptureID        string       `json:"captureId"`
		Source           model.Source `json:"source"`
		TimestampSeconds int          `json:"timestampSeconds"`
		SpanSeconds      int          `json:"spanSeconds"`
		FPS              int          `json:"fps"`
	} `json:"jobs"`
}

func (s *Server) handleSubmitJobs(w http.ResponseWriter, r *http.Request) {
	var req submitJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	type accepted struct {
		CaptureID string         `json:"captureId"`
		State     model.JobState `json:"state"`
	}
	results := make([]accepted, 0, len(req.Jobs))
	acceptedCount, skippedCount := 0, 0

	for _, item := range req.Jobs {
		if item.CaptureID == "" {
			writeError(w, http.StatusBadRequest, "every job needs a non-empty captureId")
			return
		}
		if item.Source.ServerID == "" || item.Source.ItemID == "" {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("job %q is missing source.serverId or source.itemId", item.CaptureID))
			return
		}
		// Deliberately NOT validated against s.store.GetServer here any
		// more (2026-08-26, real report: a job submitted with an unknown
		// serverId — e.g. ClipHanger has no server configured at all yet
		// — used to hard-reject the WHOLE request with a bare HTTP 400
		// and no job record ever created, leaving no trace in the Jobs
		// page at all; the only way to see why was a raw HTTP response a
		// self-hosted user's client app may not surface, or (until the
		// same-day logging fix) not even the server's own logs. This
		// exact resolution failure ALREADY has correct handling one step
		// downstream — queue.process's own `q.store.GetServer` check
		// fails the job with a clear reason once a worker picks it up —
		// so accepting the job here and letting it reach the queue
		// reuses that existing, already-correct path instead of
		// duplicating it, and gives a real, visible "failed" Jobs-page
		// entry with a real reason instead of a client-side-only error.
		job := model.Job{
			CaptureID:        item.CaptureID,
			Source:           item.Source,
			TimestampSeconds: item.TimestampSeconds,
			SpanSeconds:      item.SpanSeconds,
			FPS:              item.FPS,
		}
		if job.SpanSeconds <= 0 {
			job.SpanSeconds = model.DefaultSpanSeconds
		}
		if job.FPS <= 0 {
			job.FPS = model.DefaultFPS
		}

		state, wasAccepted := s.store.SubmitJob(job, req.Force)
		if wasAccepted {
			acceptedCount++
		} else {
			skippedCount++
		}
		results = append(results, accepted{CaptureID: item.CaptureID, State: state})
	}

	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"accepted": acceptedCount,
		"skipped":  skippedCount,
		"jobs":     results,
	})
}

// --- GET /jobs ---

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	stateFilter := model.JobState(r.URL.Query().Get("state"))
	var since time.Time
	if sinceParam := r.URL.Query().Get("since"); sinceParam != "" {
		parsed, err := time.Parse(time.RFC3339, sinceParam)
		if err != nil {
			writeError(w, http.StatusBadRequest, "since must be ISO 8601 (RFC 3339)")
			return
		}
		since = parsed
	}
	jobs := s.store.ListJobs(stateFilter, since)
	writeJSON(w, http.StatusOK, map[string]interface{}{"jobs": jobs})
}

// --- GET /jobs/{captureId} ---

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	captureID := r.PathValue("captureId")
	job, ok := s.store.GetJob(captureID)
	if !ok {
		writeError(w, http.StatusNotFound, "no job with that captureId")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// --- DELETE /jobs/{captureId} ---

func (s *Server) handleDeleteJob(w http.ResponseWriter, r *http.Request) {
	captureID := r.PathValue("captureId")
	job, ok, err := s.store.DeleteJob(captureID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "deleting job: "+err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "no job with that captureId")
		return
	}
	s.queue.DeleteMedia(job.CaptureID)
	w.WriteHeader(http.StatusNoContent)
}

// --- GET /media/{captureId}/still | /clip ---

func (s *Server) handleMedia(isClip bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		captureID := r.PathValue("captureId")
		job, ok := s.store.GetJob(captureID)
		if !ok {
			writeError(w, http.StatusNotFound, "no job with that captureId")
			return
		}
		if job.State != model.StateDone {
			writeError(w, http.StatusNotFound, fmt.Sprintf("job is %q, not done — no media to serve yet", job.State))
			return
		}

		var path, contentType string
		if isClip {
			path, contentType = s.queue.ClipPath(captureID), "video/mp4"
		} else {
			path, contentType = s.queue.StillPath(captureID), "image/jpeg"
		}

		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "job is done but the media file is missing on disk")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "reading media: "+err.Error())
			return
		}

		// ETag lets a client re-check cheaply without re-downloading
		// (docs/API.md) — the job's own UpdatedAt is a fine ETag source
		// since any regeneration bumps it.
		etag := `"` + strconv.FormatInt(job.UpdatedAt.UnixNano(), 36) + `"`
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}

		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
		http.ServeFile(w, r, filepath.Clean(path))
	}
}
