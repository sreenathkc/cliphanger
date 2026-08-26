// Package web is the credential-owning settings UI — Setup, Jobs,
// Health, Security, server-rendered and embedded in the binary. Open by
// default on the LAN, no login wall — changed 2026-08-23 (see
// docs/DECISIONS.md "Web UI is open by default, not Basic-Auth-walled"
// for the full reasoning) after direct pushback on the original design
// (HTTP Basic Auth using the API key as the password): that made first
// login genuinely circular — the credential needed to see the Setup
// page was ONLY ever shown ON the Setup page — and didn't match how
// comparable self-hosted tools (Sonarr, Radarr, Overseerr) actually
// work, which is unauthenticated by default on a trusted home LAN.
//
// Local login (2026-08-25) is an opt-in ADDITION to that decision, not a
// reversal — see store.SetLocalLogin's own doc comment. The credential
// it's configured with lives entirely inside this already-open UI (the
// Security page), the same bootstrap order Sonarr/Radarr/Overseerr
// themselves use: you set a username/password while still unauthenticated,
// THEN flip it on, so there's no circular "need the credential to reach
// the page that issues it" problem this time.
package web

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
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

// staticFS holds the favicon/PWA-manifest bundle (2026-08-24) —
// favicon.io's standard export set. See RegisterStaticAssets for why
// these are served from bare root paths rather than nested under this
// package's own /ui/ routes.
//
//go:embed static/*
var staticFS embed.FS

// RegisterStaticAssets mounts the favicon/manifest bundle onto mux at
// bare root paths — browsers request /favicon.ico (and a manifest's own
// icon entries) unprefixed, regardless of where the HTML page
// referencing them actually lives, so these can't nest under /ui/ the
// way the rest of this package's routes do. Called directly against the
// TOP-LEVEL mux in cmd/cliphanger/main.go, not this package's own
// sub-mux — see main.go's routes() comment for the /ui/ vs bare-path
// split this already follows.
func RegisterStaticAssets(mux *http.ServeMux) {
	// Go's mime package has no built-in mapping for .webmanifest, so
	// http.FileServerFS falls back to content-sniffing it as
	// text/plain — technically wrong (the Web App Manifest spec calls
	// for application/manifest+json) and enough to make some browsers/
	// PWA install prompts ignore it. Registering it process-wide fixes
	// FileServerFS's lookup for every request, not just the first.
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")

	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err) // embedded at compile time — can't fail at runtime
	}
	fileServer := http.FileServerFS(sub)
	for _, name := range []string{
		"/favicon.ico", "/favicon.svg", "/favicon-96x96.png",
		"/apple-touch-icon.png", "/web-app-manifest-192x192.png",
		"/web-app-manifest-512x512.png", "/site.webmanifest",
	} {
		mux.Handle("GET "+name, fileServer)
	}
}

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
	//
	// /login and /logout are deliberately the only routes NOT wrapped in
	// requireLogin — wrapping them would make the login page itself
	// require being already logged in to view, which is exactly the
	// circular bootstrap problem this feature's own package doc comment
	// explains how it avoids. Every other route below gets requireLogin,
	// which is a no-op pass-through unless local login is actually turned
	// on (see that function's own comment) — so this doesn't change
	// anything about how the UI behaves until a user opts in on the
	// Security page.
	s.mux.HandleFunc("GET /login", s.withLogging(s.handleLoginPage))
	s.mux.HandleFunc("POST /login", s.withLogging(s.handleLoginSubmit))
	s.mux.HandleFunc("POST /logout", s.withLogging(s.handleLogout))

	s.mux.HandleFunc("GET /setup", s.withLogging(s.requireLogin(s.handleSetup)))
	s.mux.HandleFunc("POST /setup/servers", s.withLogging(s.requireLogin(s.handleAddServer)))
	s.mux.HandleFunc("POST /setup/plex/pin", s.withLogging(s.requireLogin(s.handleCreatePlexPin)))
	s.mux.HandleFunc("GET /setup/plex/pin/{id}", s.withLogging(s.requireLogin(s.handlePollPlexPin)))
	s.mux.HandleFunc("POST /setup/plex/servers", s.withLogging(s.requireLogin(s.handleListPlexServers)))
	s.mux.HandleFunc("POST /setup/servers/{id}/test", s.withLogging(s.requireLogin(s.handleTestServer)))
	s.mux.HandleFunc("POST /setup/servers/{id}/delete", s.withLogging(s.requireLogin(s.handleDeleteServer)))
	s.mux.HandleFunc("POST /setup/retention", s.withLogging(s.requireLogin(s.handleSetRetention)))
	s.mux.HandleFunc("POST /setup/clip-duration", s.withLogging(s.requireLogin(s.handleSetClipDuration)))
	s.mux.HandleFunc("POST /setup/speed-multiplier", s.withLogging(s.requireLogin(s.handleSetSpeedMultiplier)))
	s.mux.HandleFunc("POST /setup/max-concurrent-jobs", s.withLogging(s.requireLogin(s.handleSetMaxConcurrentJobs)))
	s.mux.HandleFunc("GET /jobs", s.withLogging(s.requireLogin(s.handleJobs)))
	s.mux.HandleFunc("POST /jobs/{captureId}/delete", s.withLogging(s.requireLogin(s.handleDeleteJob)))
	s.mux.HandleFunc("GET /jobs/{captureId}/thumb", s.withLogging(s.requireLogin(s.handleThumb)))
	s.mux.HandleFunc("GET /jobs/{captureId}/log", s.withLogging(s.requireLogin(s.handleJobLog)))
	s.mux.HandleFunc("GET /health", s.withLogging(s.requireLogin(s.handleHealth)))

	// Security — API key + local login, split out of Setup 2026-08-25
	// (per direct license to reorganize: "you can change the menu items/
	// organise if the setup page is getting too busy") once Setup's own
	// six sections plus a new Local Login section would have made seven
	// on one page. Grouping the API key here too, not just the new
	// setting, groups everything that's actually about "who can get in"
	// under one tab instead of splitting it across two.
	s.mux.HandleFunc("GET /security", s.withLogging(s.requireLogin(s.handleSecurity)))
	s.mux.HandleFunc("POST /security/apikey/rotate", s.withLogging(s.requireLogin(s.handleRotateKey)))
	s.mux.HandleFunc("POST /security/login", s.withLogging(s.requireLogin(s.handleSetLocalLogin)))
}

// requireLogin gates a route behind a valid session cookie — but ONLY
// when local login is actually enabled in the store; otherwise it's a
// transparent pass-through, so nothing changes about the "open by
// default on the LAN" behavior until a user explicitly opts in on the
// Security page. Checked fresh on every request (not cached) so flipping
// the toggle off takes effect immediately, with no stale "still
// enabled" state anywhere to get out of sync.
func (s *Server) requireLogin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.store.LocalLoginEnabled() {
			next(w, r)
			return
		}
		cookie, err := r.Cookie(sessionCookieName)
		if err == nil {
			secret, secretErr := s.store.SessionSecret()
			if secretErr == nil {
				if _, ok := verifySession(secret, cookie.Value); ok {
					next(w, r)
					return
				}
			}
		}
		// r.URL.Path here is already /ui-stripped (cmd/cliphanger/main.go
		// mounts this whole package under /ui/ via http.StripPrefix) — the
		// prefix has to be added back for `next` to be a real, reachable
		// URL rather than one only valid from inside this package's own
		// (prefix-less) view of its routes.
		http.Redirect(w, r, "/ui/login?next="+template.URLQueryEscaper("/ui"+r.URL.Path), http.StatusFound)
	}
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

// withLogging used to be requireAuth — the web UI's own auth is
// requireLogin now (2026-08-25), a separate, opt-in wrapper (see that
// function's comment), not this one. This one is purely the
// request-level logging every route relies on regardless of auth state
// (added 2026-08-23, after a real report that "nothing happened" in the
// browser with no way to tell, server-side, whether the request even
// arrived), so it stays as the outermost wrapper every route goes
// through — a redirected-to-login request still gets logged. Method/
// path/status/duration only, same as before.
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
	flash, flashErr := flashFromQuery(r)
	s.render(w, "setup", map[string]interface{}{
		"Title": "Setup", "Nav": "setup",
		"Flash": flash, "FlashError": flashErr,
		"Servers":                  s.store.ListServers(),
		"RetentionHours":           s.store.RetentionHours(),
		"DefaultSpanSeconds":       s.store.DefaultSpanSeconds(),
		"DefaultSpeedMultiplier":   s.store.DefaultSpeedMultiplier(),
		"DefaultFPS":               model.DefaultFPS,
		"MaxConcurrentJobs":        s.store.MaxConcurrentJobs(),
		"MaxConcurrentJobsIsAuto":  s.store.MaxConcurrentJobsIsAuto(),
		"MaxConcurrentJobsCeiling": model.MaxConcurrentJobsCeiling,
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

// handleSetSpeedMultiplier is the Setup page's playback-speed "Save"
// action (2026-08-24, per direct request — "an option to make the clip
// speed faster... to preview more of the scene in less time"). Same
// shape as handleSetClipDuration. Bounded 1–8x: past that the source
// read for a 20s clip would exceed 2+ minutes for very little
// additional legibility in the compressed result.
func (s *Server) handleSetSpeedMultiplier(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		redirectWithFlash(w, r, "/ui/setup", "Couldn't read that form submission.", true)
		return
	}
	multiplier, err := strconv.Atoi(r.FormValue("speedMultiplier"))
	if err != nil || multiplier < 1 || multiplier > 8 {
		redirectWithFlash(w, r, "/ui/setup", "Playback speed must be a whole number, 1–8x.", true)
		return
	}
	if err := s.store.SetDefaultSpeedMultiplier(multiplier); err != nil {
		redirectWithFlash(w, r, "/ui/setup", "Couldn't save playback speed: "+err.Error(), true)
		return
	}
	redirectWithFlash(w, r, "/ui/setup", fmt.Sprintf("Default playback speed set to %dx.", multiplier), false)
}

// handleSetMaxConcurrentJobs is the Setup page's concurrency "Save"
// action (2026-08-26, per direct report — "i just queued up 4 and it
// is running longer, so by default lets set to 1 or auto"). Blank or 0
// means Auto (hardware-detected, see Store.AutoMaxConcurrentJobs);
// takes effect on the queue's very next job pull, no restart needed —
// same live pattern as retention/clip-duration/speed above.
func (s *Server) handleSetMaxConcurrentJobs(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		redirectWithFlash(w, r, "/ui/setup", "Couldn't read that form submission.", true)
		return
	}
	raw := r.FormValue("maxConcurrentJobs")
	n := 0
	if raw != "" {
		var err error
		n, err = strconv.Atoi(raw)
		if err != nil || n < 0 || n > model.MaxConcurrentJobsCeiling {
			redirectWithFlash(w, r, "/ui/setup", fmt.Sprintf("Max concurrent jobs must be blank/0 (Auto) or a whole number 1-%d.", model.MaxConcurrentJobsCeiling), true)
			return
		}
	}
	if err := s.store.SetMaxConcurrentJobs(n); err != nil {
		redirectWithFlash(w, r, "/ui/setup", "Couldn't save max concurrent jobs: "+err.Error(), true)
		return
	}
	msg := fmt.Sprintf("Max concurrent jobs set to Auto (currently %d, based on this host's CPU count).", store.AutoMaxConcurrentJobs())
	if n > 0 {
		msg = fmt.Sprintf("Max concurrent jobs set to %d.", n)
	}
	redirectWithFlash(w, r, "/ui/setup", msg, false)
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
		redirectWithFlash(w, r, "/ui/security", "Couldn't rotate the key: "+err.Error(), true)
		return
	}
	redirectWithFlash(w, r, "/ui/security", "API key rotated. Update every client before they try again.", false)
}

// --- Security (2026-08-25) ---

func (s *Server) handleSecurity(w http.ResponseWriter, r *http.Request) {
	key, err := s.store.APIKey()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	flash, flashErr := flashFromQuery(r)
	s.render(w, "security", map[string]interface{}{
		"Title": "Security", "Nav": "security",
		"Flash": flash, "FlashError": flashErr,
		"APIKey":            key,
		"LocalLoginEnabled": s.store.LocalLoginEnabled(),
		"LoginUsername":     s.store.LoginUsername(),
		"HasStoredPassword": s.store.HasStoredPassword(),
	})
}

// handleSetLocalLogin is the Security page's Local Login "Save" action.
// See store.SetLocalLogin's own doc comment for the validation rules
// (username+password both required to enable; blank password keeps the
// existing one) — this handler just reads the form and reports whatever
// that returns, success or the specific reason it refused.
func (s *Server) handleSetLocalLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		redirectWithFlash(w, r, "/ui/security", "Couldn't read that form submission.", true)
		return
	}
	enabled := r.FormValue("enabled") == "on"
	username := r.FormValue("username")
	password := r.FormValue("password")
	if err := s.store.SetLocalLogin(enabled, username, password); err != nil {
		redirectWithFlash(w, r, "/ui/security", err.Error(), true)
		return
	}
	msg := "Local login turned off — the web UI is open on your network again."
	if enabled {
		msg = "Local login is on. You'll need to sign in on this and any other browser from now on."
	}
	redirectWithFlash(w, r, "/ui/security", msg, false)
}

// --- Login (2026-08-25) ---
//
// Deliberately outside requireLogin (see routes()) — these are the one
// path that has to stay reachable regardless of login state, or turning
// local login on would make it impossible to ever log in at all.

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	// Already logged in (or login isn't even on) and landed here anyway
	// — e.g. a bookmarked /ui/login, or clicking back after signing in —
	// send them somewhere that actually has content rather than showing
	// a login form there's nothing left to do with.
	if !s.store.LocalLoginEnabled() {
		http.Redirect(w, r, "/ui/setup", http.StatusFound)
		return
	}
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		if secret, err := s.store.SessionSecret(); err == nil {
			if _, ok := verifySession(secret, cookie.Value); ok {
				http.Redirect(w, r, "/ui/setup", http.StatusFound)
				return
			}
		}
	}
	next := r.URL.Query().Get("next")
	if next == "" {
		next = "/ui/setup"
	}
	flash, flashErr := flashFromQuery(r)
	s.render(w, "login", map[string]interface{}{
		"Title": "Log In", "HideNav": true, "Next": next,
		"Flash": flash, "FlashError": flashErr,
	})
}

// loginRedirect builds a /ui/login URL carrying both `next` (where to
// land after a successful attempt) and a flash message — a small
// variant of redirectWithFlash, which always appends its own query
// string with a literal "?" and would produce an invalid URL
// (`?next=X?flash=Y`) if handed a path that already has one.
func loginRedirect(w http.ResponseWriter, r *http.Request, next, message string, isError bool) {
	q := url.Values{"next": {next}, "flash": {message}}
	if isError {
		q.Set("flashError", "1")
	}
	http.Redirect(w, r, "/ui/login?"+q.Encode(), http.StatusFound)
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		loginRedirect(w, r, "/ui/setup", "Couldn't read that form submission.", true)
		return
	}
	username := r.FormValue("username")
	password := r.FormValue("password")
	next := r.FormValue("next")
	if next == "" {
		next = "/ui/setup"
	}
	if !s.store.VerifyLogin(username, password) {
		loginRedirect(w, r, next, "Wrong username or password.", true)
		return
	}
	secret, err := s.store.SessionSecret()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	expiry := time.Now().Add(sessionDuration)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: signSession(secret, username, expiry),
		Path: "/", Expires: expiry, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, next, http.StatusFound)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "",
		Path: "/", Expires: time.Unix(0, 0), HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/ui/login", http.StatusFound)
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
