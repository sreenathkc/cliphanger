// Package web is the credential-owning settings UI — Setup, Jobs,
// Health, server-rendered and embedded in the binary. Open by default
// on the LAN, no login wall — changed 2026-08-23 (see
// docs/DECISIONS.md "Web UI is open by default, not Basic-Auth-walled"
// for the full reasoning) after direct pushback on the original design
// (HTTP Basic Auth using the API key as the password): that made first
// login genuinely circular — the credential needed to see the Setup
// page was ONLY ever shown ON the Setup page — and didn't match how
// comparable self-hosted tools (Sonarr, Radarr, Overseerr) actually
// work, which is unauthenticated by default on a trusted home LAN.
package web

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/sreenathkc/cliphanger/internal/backend"
	"github.com/sreenathkc/cliphanger/internal/extract"
	"github.com/sreenathkc/cliphanger/internal/model"
	"github.com/sreenathkc/cliphanger/internal/plexauth"
	"github.com/sreenathkc/cliphanger/internal/queue"
	"github.com/sreenathkc/cliphanger/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

type Server struct {
	store    *store.Store
	queue    *queue.Queue
	registry *backend.Registry
	mediaDir string
	dataDir  string
	logger   *slog.Logger
	mux      *http.ServeMux
	tmpl     *template.Template
}

func New(st *store.Store, q *queue.Queue, registry *backend.Registry, mediaDir, dataDir string, logger *slog.Logger) (*Server, error) {
	tmpl, err := template.ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parsing web templates: %w", err)
	}
	s := &Server{store: st, queue: q, registry: registry, mediaDir: mediaDir, dataDir: dataDir, logger: logger, mux: http.NewServeMux(), tmpl: tmpl}
	s.routes()
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	// No root ("/") route here — this Server is mounted under /ui/ by
	// cmd/cliphanger/main.go (via http.StripPrefix), which registers
	// its own top-level "/" → "/ui/setup" redirect instead. Registering
	// one here too would only ever be reached at literal "/ui/", not
	// the bare site root a browser actually opens.
	s.mux.HandleFunc("GET /setup", s.withLogging(s.handleSetup))
	s.mux.HandleFunc("POST /setup/servers", s.withLogging(s.handleAddServer))
	s.mux.HandleFunc("POST /setup/plex/pin", s.withLogging(s.handleCreatePlexPin))
	s.mux.HandleFunc("GET /setup/plex/pin/{id}", s.withLogging(s.handlePollPlexPin))
	s.mux.HandleFunc("POST /setup/plex/servers", s.withLogging(s.handleListPlexServers))
	s.mux.HandleFunc("POST /setup/servers/{id}/test", s.withLogging(s.handleTestServer))
	s.mux.HandleFunc("POST /setup/servers/{id}/delete", s.withLogging(s.handleDeleteServer))
	s.mux.HandleFunc("POST /setup/apikey/rotate", s.withLogging(s.handleRotateKey))
	s.mux.HandleFunc("POST /setup/retention", s.withLogging(s.handleSetRetention))
	s.mux.HandleFunc("POST /setup/clip-duration", s.withLogging(s.handleSetClipDuration))
	s.mux.HandleFunc("GET /jobs", s.withLogging(s.handleJobs))
	s.mux.HandleFunc("POST /jobs/{captureId}/delete", s.withLogging(s.handleDeleteJob))
	s.mux.HandleFunc("GET /jobs/{captureId}/thumb", s.withLogging(s.handleThumb))
	s.mux.HandleFunc("GET /jobs/{captureId}/log", s.withLogging(s.handleJobLog))
	s.mux.HandleFunc("GET /health", s.withLogging(s.handleHealth))
}

// statusRecorder captures the status code a handler actually wrote —
// http.ResponseWriter has no getter for it otherwise, and request
// logging (below) needs it.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// withLogging used to be requireAuth — the web UI is no longer behind
// Basic Auth (see package doc comment / docs/DECISIONS.md), but the
// request-level logging every route relies on (added 2026-08-23, after
// a real report that "nothing happened" in the browser with no way to
// tell, server-side, whether the request even arrived) is still
// valuable regardless, so it stays as the one wrapper every route goes
// through. Method/path/status/duration only, same as before.
func (s *Server) withLogging(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			s.logger.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.status, "duration", time.Since(start))
		}()
		next(rec, r)
	}
}

// render executes the page-specific template named `name` (one of
// "setup", "jobs", "health" — matching each html file's own
// {{define}}) into a buffer, then hands the result to "layout" as
// .Content (bug fixed 2026-08-22 — "why does all 3 tabs show same
// content?"). The three page templates all used to {{define "content"}}
// under the SAME name; template.ParseFS loads every file into one
// shared namespace, so whichever file happened to parse last silently
// overwrote the other two's block, and layout.html's `{{template
// "content" .}}` always resolved to just that one regardless of which
// handler actually ran. Rendering the named page template explicitly
// first, THEN injecting its already-rendered HTML into the layout,
// means there's no shared name left to collide on.
func (s *Server) render(w http.ResponseWriter, name string, data map[string]interface{}) {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		s.logger.Error("page render failed", "template", name, "error", err)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	data["Content"] = template.HTML(buf.String())

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "layout", data); err != nil {
		s.logger.Error("layout render failed", "error", err)
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

func flashFromQuery(r *http.Request) (string, bool) {
	if msg := r.URL.Query().Get("flash"); msg != "" {
		return msg, r.URL.Query().Get("flashError") == "1"
	}
	return "", false
}

func redirectWithFlash(w http.ResponseWriter, r *http.Request, path, message string, isError bool) {
	errParam := "0"
	if isError {
		errParam = "1"
	}
	http.Redirect(w, r, fmt.Sprintf("%s?flash=%s&flashError=%s", path, template.URLQueryEscaper(message), errParam), http.StatusFound)
}

// --- Setup ---

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	key, err := s.store.APIKey()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	flash, flashErr := flashFromQuery(r)
	s.render(w, "setup", map[string]interface{}{
		"Title": "Setup", "Nav": "setup",
		"Flash": flash, "FlashError": flashErr,
		"APIKey": key, "Servers": s.store.ListServers(),
		"RetentionHours":     s.store.RetentionHours(),
		"DefaultSpanSeconds": s.store.DefaultSpanSeconds(),
	})
}

// handleSetRetention is the Setup page's retention "Save" action
// (2026-08-22, per direct request — "by default forever, user can
// change in settings"). 0 (or blank) means forever; takes effect on
// the queue's next sweep, no restart needed.
func (s *Server) handleSetRetention(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		redirectWithFlash(w, r, "/ui/setup", "Couldn't read that form submission.", true)
		return
	}
	hours, err := strconv.Atoi(r.FormValue("retentionHours"))
	if err != nil || hours < 0 {
		redirectWithFlash(w, r, "/ui/setup", "Retention must be a whole number of hours (0 = forever).", true)
		return
	}
	if err := s.store.SetRetentionHours(hours); err != nil {
		redirectWithFlash(w, r, "/ui/setup", "Couldn't save retention: "+err.Error(), true)
		return
	}
	msg := "Retention set to keep jobs forever."
	if hours > 0 {
		msg = fmt.Sprintf("Retention set to %d hour(s).", hours)
	}
	redirectWithFlash(w, r, "/ui/setup", msg, false)
}

// handleSetClipDuration is the Setup page's clip-duration "Save" action
// (2026-08-23, per direct request — "add the option to adjust the
// duration of the movie clip needs to be generated"). Applies to any
// job that omits its own spanSeconds; a job that specifies one
// explicitly (as DemoFlex itself always does) is unaffected — see
// ClipHangerMediaClient.swift. Bounded 1–120s: unbounded would let a
// value slip in that produces a multi-minute-long "preview" clip, which
// is not what this feature is for.
func (s *Server) handleSetClipDuration(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		redirectWithFlash(w, r, "/ui/setup", "Couldn't read that form submission.", true)
		return
	}
	seconds, err := strconv.Atoi(r.FormValue("clipDurationSeconds"))
	if err != nil || seconds < 1 || seconds > 120 {
		redirectWithFlash(w, r, "/ui/setup", "Clip duration must be a whole number of seconds, 1–120.", true)
		return
	}
	if err := s.store.SetDefaultSpanSeconds(seconds); err != nil {
		redirectWithFlash(w, r, "/ui/setup", "Couldn't save clip duration: "+err.Error(), true)
		return
	}
	redirectWithFlash(w, r, "/ui/setup", fmt.Sprintf("Default clip duration set to %ds.", seconds), false)
}

func (s *Server) handleAddServer(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		redirectWithFlash(w, r, "/ui/setup", "Couldn't read that form submission.", true)
		return
	}
	kind := model.ServerKind(r.FormValue("kind"))
	if kind != model.KindPlex && kind != model.KindKodi && kind != model.KindJellyfin {
		redirectWithFlash(w, r, "/ui/setup", "Unknown server kind.", true)
		return
	}
	port, _ := strconv.Atoi(r.FormValue("port"))
	srv := model.Server{
		Kind:          kind,
		Name:          r.FormValue("name"),
		Host:          r.FormValue("host"),
		Port:          port,
		PlexToken:     r.FormValue("plexToken"),
		KodiUsername:  r.FormValue("kodiUsername"),
		KodiPassword:  r.FormValue("kodiPassword"),
		JellyfinToken: r.FormValue("jellyfinToken"),
		LocalPathFrom: r.FormValue("localPathFrom"),
		LocalPathTo:   r.FormValue("localPathTo"),
	}
	if srv.Name == "" || srv.Host == "" || srv.Port == 0 {
		redirectWithFlash(w, r, "/ui/setup", "Name, host and port are all required.", true)
		return
	}

	// Test BEFORE saving, not after (bug fixed 2026-08-22, per direct
	// request — "the UI should add a connection only after testing it").
	// A server that never actually gets a "Test connection" click used
	// to sit in the list looking configured, with nothing to distinguish
	// it from a real one until a job against it failed.
	be, err := s.registry.For(kind)
	if err != nil {
		redirectWithFlash(w, r, "/ui/setup", err.Error(), true)
		return
	}
	if testErr := be.TestConnection(r.Context(), srv); testErr != nil {
		redirectWithFlash(w, r, "/ui/setup", fmt.Sprintf("Couldn't reach %q — not added: %s", srv.Name, testErr.Error()), true)
		return
	}
	srv.LastReachable = true
	srv.LastCheckedAt = time.Now().UTC()

	saved, err := s.store.PutServer(srv)
	if err != nil {
		redirectWithFlash(w, r, "/ui/setup", "Connection verified, but couldn't save that server: "+err.Error(), true)
		return
	}
	redirectWithFlash(w, r, "/ui/setup", fmt.Sprintf("Added %q — connection verified.", saved.Name), false)
}

// --- Sign in with Plex (2026-08-22) ---
//
// Two small JSON endpoints the setup page's own JS calls directly
// (fetch, not a form POST-redirect like the rest of this file).

func (s *Server) handleCreatePlexPin(w http.ResponseWriter, r *http.Request) {
	clientID, err := s.store.ClientIdentifier()
	if err != nil {
		writeWebJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	session, err := plexauth.CreatePin(r.Context(), clientID, "ClipHanger")
	if err != nil {
		writeWebJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeWebJSON(w, http.StatusOK, map[string]interface{}{
		"pinId":   session.ID,
		"authUrl": session.AuthURL,
	})
}

func (s *Server) handlePollPlexPin(w http.ResponseWriter, r *http.Request) {
	pinID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeWebJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid pin id"})
		return
	}
	clientID, err := s.store.ClientIdentifier()
	if err != nil {
		writeWebJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	token, err := plexauth.Poll(r.Context(), clientID, pinID)
	if err != nil {
		writeWebJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if token == "" {
		writeWebJSON(w, http.StatusOK, map[string]string{"status": "pending"})
		return
	}
	writeWebJSON(w, http.StatusOK, map[string]string{"status": "done", "token": token})
}

// handleListPlexServers is the other half of "Sign in with Plex"
// actually doing what it says (2026-08-22, per direct report — "the
// whole point of Sign in with Plex is to auto login and fill details,
// same like the iOS app"): the pin flow above only ever fetched a
// TOKEN, leaving host/port to be typed in by hand regardless — the
// exact thing DemoFlex's own iOS sign-in already doesn't require. Takes
// the token as a POST body (not a query param) so it never lands in a
// server access log.
func (s *Server) handleListPlexServers(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token == "" {
		writeWebJSON(w, http.StatusBadRequest, map[string]string{"error": "missing token"})
		return
	}
	clientID, err := s.store.ClientIdentifier()
	if err != nil {
		writeWebJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	servers, err := plexauth.FetchServers(r.Context(), clientID, body.Token)
	if err != nil {
		writeWebJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeWebJSON(w, http.StatusOK, map[string]interface{}{"servers": servers})
}

func writeWebJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handleTestServer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	srv, ok := s.store.GetServer(id)
	if !ok {
		redirectWithFlash(w, r, "/ui/setup", "That server no longer exists.", true)
		return
	}
	be, err := s.registry.For(srv.Kind)
	if err != nil {
		redirectWithFlash(w, r, "/ui/setup", err.Error(), true)
		return
	}
	testErr := be.TestConnection(r.Context(), srv)
	_ = s.store.SetServerReachability(id, testErr == nil)
	if testErr != nil {
		redirectWithFlash(w, r, "/ui/setup", fmt.Sprintf("%s: %s", srv.Name, testErr.Error()), true)
		return
	}
	redirectWithFlash(w, r, "/ui/setup", fmt.Sprintf("%s is reachable.", srv.Name), false)
}

func (s *Server) handleDeleteServer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteServer(id); err != nil {
		redirectWithFlash(w, r, "/ui/setup", "Couldn't remove that server: "+err.Error(), true)
		return
	}
	redirectWithFlash(w, r, "/ui/setup", "Server removed.", false)
}

func (s *Server) handleRotateKey(w http.ResponseWriter, r *http.Request) {
	if _, err := s.store.RotateAPIKey(); err != nil {
		redirectWithFlash(w, r, "/ui/setup", "Couldn't rotate the key: "+err.Error(), true)
		return
	}
	redirectWithFlash(w, r, "/ui/setup", "API key rotated. Update every client before they try again.", false)
}

// --- Jobs ---

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	stateFilter := model.JobState(r.URL.Query().Get("state"))
	jobs := s.store.ListJobs(stateFilter, time.Time{})
	flash, flashErr := flashFromQuery(r)
	s.render(w, "jobs", map[string]interface{}{
		"Title": "Jobs", "Nav": "jobs",
		"Flash": flash, "FlashError": flashErr,
		"Jobs": jobs, "StateFilter": string(stateFilter),
	})
}

func (s *Server) handleDeleteJob(w http.ResponseWriter, r *http.Request) {
	captureID := r.PathValue("captureId")
	job, ok, err := s.store.DeleteJob(captureID)
	if err != nil {
		redirectWithFlash(w, r, "/ui/jobs", "Couldn't delete that job: "+err.Error(), true)
		return
	}
	if ok {
		s.queue.DeleteMedia(job.CaptureID)
	}
	redirectWithFlash(w, r, "/ui/jobs", "Job deleted.", false)
}

func (s *Server) handleThumb(w http.ResponseWriter, r *http.Request) {
	captureID := r.PathValue("captureId")
	job, ok := s.store.GetJob(captureID)
	if !ok || job.State != model.StateDone {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	path := s.queue.StillPath(captureID)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	http.ServeFile(w, r, filepath.Clean(path))
}

// handleJobLog backs jobs.html's "view live log" toggle (2026-08-22, per
// direct request — "we need to show the execution live log also
// somewhere if user wants to see"). Polled by the page's own JS while a
// running job's log is expanded; see queue.Queue.LiveLog's comment for
// why "running: false" covers queued/done/failed/unknown alike — the
// page's answer is the same either way, so there's nothing finer to
// report here.
func (s *Server) handleJobLog(w http.ResponseWriter, r *http.Request) {
	captureID := r.PathValue("captureId")
	log, running := s.queue.LiveLog(captureID)
	writeWebJSON(w, http.StatusOK, map[string]interface{}{
		"log":     log,
		"running": running,
	})
}

// --- Health ---

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	queued, running, failed := s.store.Counts()
	usage := diskUsage(s.mediaDir)
	retention := "forever — done/failed jobs are kept until manually deleted"
	if hours := s.store.RetentionHours(); hours > 0 {
		retention = fmt.Sprintf("%dh — done/failed jobs are deleted after this (change on the Setup page)", hours)
	}
	s.render(w, "health", map[string]interface{}{
		"Title": "Health", "Nav": "health",
		"DataDir":        s.dataDir,
		"MediaDiskUsage": formatBytes(usage),
		"Retention":      retention,
		"Servers":        s.store.ListServers(),
		"Health": map[string]interface{}{
			"Service": "cliphanger", "Version": "0.1.0",
			"FFmpeg": extract.Version(r.Context()),
			"Queued": queued, "Running": running, "Failed": failed,
		},
	})
}

func diskUsage(dir string) int64 {
	var total int64
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil {
			total += info.Size()
		}
	}
	return total
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
