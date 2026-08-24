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
	store *store.Store
	queue *queue.Queue
	mux   *http.ServeMux
}

func New(st *store.Store, q *queue.Queue) *Server {
	s := &Server{store: st, queue: q, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /health", s.requireAPIKey(s.handleHealth))
	s.mux.HandleFunc("GET /servers", s.requireAPIKey(s.handleListServers))
	s.mux.HandleFunc("POST /jobs", s.requireAPIKey(s.handleSubmitJobs))
	s.mux.HandleFunc("GET /jobs", s.requireAPIKey(s.handleListJobs))
	s.mux.HandleFunc("GET /jobs/{captureId}", s.requireAPIKey(s.handleGetJob))
	s.mux.HandleFunc("DELETE /jobs/{captureId}", s.requireAPIKey(s.handleDeleteJob))
	s.mux.HandleFunc("GET /media/{captureId}/still", s.requireAPIKey(s.handleMedia(false)))
	s.mux.HandleFunc("GET /media/{captureId}/clip", s.requireAPIKey(s.handleMedia(true)))
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
		if _, ok := s.store.GetServer(item.Source.ServerID); !ok {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("job %q references unknown serverId %q", item.CaptureID, item.Source.ServerID))
			return
		}

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
