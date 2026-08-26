// Package store is ClipHanger's persistence — deliberately a single
// JSON file, not a database. This is a self-hosted, single-instance
// tool (docs/DECISIONS.md "Open questions" notes multiple instances
// isn't supported yet); a home-lab user's whole queue and server list
// is a few KB, and a plain file means `docker cp`, backup, and manual
// inspection all just work with tools everyone already has. Durability
// matters (a restart shouldn't lose the queue), live query performance
// does not (this is polled every few seconds by one or two clients, not
// hammered).
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/sreenathkc/cliphanger/internal/model"
)

// Store is safe for concurrent use — every mutating method takes the
// same mutex, and reads take a read lock. Writes go through
// writeLocked, which always rewrites the whole file: simple, and cheap
// enough at this scale that no incremental-write complexity is worth
// it.
type Store struct {
	mu   sync.RWMutex
	path string
	data diskData
}

type diskData struct {
	Servers []model.Server `json:"servers"`
	Jobs    []model.Job    `json:"jobs"`
	APIKey  string         `json:"apiKey"`
	// ClientIdentifier is THIS ClipHanger install's own stable identity
	// with plex.tv, used for the "Sign in with Plex" PIN/link flow
	// (2026-08-22) — plex.tv ties a pin (and the token it eventually
	// yields) to whichever X-Plex-Client-Identifier created it, so this
	// has to be generated once and persisted, the same reasoning as
	// APIKey. Unrelated to any individual media server's own identity.
	ClientIdentifier string `json:"clientIdentifier"`

	// RetentionHours governs the queue's retention sweep — 0 means
	// "forever," which is also Go's zero value, so a store that's never
	// touched this defaults to forever with no special-casing needed.
	// Changed 2026-08-22, per direct request ("by default forever, user
	// can change in settings"): this used to be read once from
	// JOB... RETENTION_HOURS at startup and fixed for the process's
	// lifetime; it's now a persisted, live-editable setting (Setup
	// page), so a change takes effect on the next sweep without a
	// restart. RetentionConfigured distinguishes "genuinely never set"
	// from "explicitly set to 0" — needed so the RETENTION_HOURS env var
	// can still seed a non-default starting value on a fresh install
	// (SeedRetentionHoursIfUnset, mirroring SeedAPIKeyIfUnset) without
	// that seed re-applying on every restart and silently undoing
	// whatever the user set via the UI afterward.
	RetentionHours      int  `json:"retentionHours"`
	RetentionConfigured bool `json:"retentionConfigured"`

	// DefaultSpanSeconds is what a job's own spanSeconds falls back to
	// when a client omits it (2026-08-23, per direct request — "add the
	// option to adjust the duration of the movie clip... default is
	// 20"), the Setup page's live-editable clip-duration setting. Unlike
	// RetentionHours, 0 is never itself a meaningful clip duration, so
	// there's no separate "configured" flag needed to disambiguate an
	// explicit 0 from "never touched" — 0/unset just means "use
	// model.DefaultSpanSeconds," full stop.
	DefaultSpanSeconds int `json:"defaultSpanSeconds"`

	// DefaultSpeedMultiplier: same "0/unset means use the model
	// constant" pattern as DefaultSpanSeconds above (2026-08-24, per
	// direct request — "an option to make the clip speed faster... to
	// preview more of the scene in less time"). Setup-page only; no
	// per-job client override exists yet.
	DefaultSpeedMultiplier int `json:"defaultSpeedMultiplier"`

	// MaxConcurrentJobs caps how many jobs the queue runs at once — 0
	// means "Auto" (detect from the host's CPU count, see
	// AutoMaxConcurrentJobs), which is also Go's zero value, so a store
	// that's never touched this defaults to Auto with no special-casing
	// needed. Added 2026-08-26, per direct report: "i just queued up 4
	// and it is running longer." WORKERS used to be a fixed env var
	// (default 3), read once at startup and hard-coded ever after — no
	// live Setup-page control, and no hardware awareness at all on a
	// self-hosted box that could be anything from a 2-core NAS to a
	// beefy home server. More workers doesn't mean more throughput past
	// what the host can actually decode in parallel — see
	// model.MaxConcurrentJobsCeiling's own comment — so a fixed default
	// tuned for nobody in particular was exactly what produced the
	// report: 3 parallel ffmpeg decodes on hardware that could
	// comfortably do 1 or 2, each one slower than if it had run alone.
	// MaxConcurrentJobsConfigured distinguishes "genuinely never set"
	// from "explicitly set to 0/Auto" — same reasoning as
	// RetentionConfigured, needed so the WORKERS env var can still seed
	// a starting value on a fresh install without re-applying on every
	// restart and silently undoing an explicit choice made via the
	// Setup page.
	MaxConcurrentJobs           int  `json:"maxConcurrentJobs"`
	MaxConcurrentJobsConfigured bool `json:"maxConcurrentJobsConfigured"`

	// --- Local login (2026-08-25, per direct request: "when I look at
	// self-hosted apps like Radarr etc, they have an option to enable
	// local login, once enabled local user will need to login even while
	// accessing locally. we will need that as well.") A SEPARATE access
	// gate from APIKey above, not a replacement — this is only ever
	// checked by package web's own HTML pages (Setup/Jobs/Health/
	// Security), never by package api's JSON routes, which DemoFlex and
	// any other API client keep authenticating to with the API key alone,
	// completely unaffected by whether this is turned on. Off by default,
	// preserving the "Web UI is open by default" decision (docs/
	// DECISIONS.md) — this is an opt-in ADDITION to that decision, not a
	// reversal of it.
	LocalLoginEnabled bool `json:"localLoginEnabled"`
	// LoginUsername is plain text — usernames aren't secret. LoginPasswordHash
	// is bcrypt, never the raw password.
	LoginUsername     string `json:"loginUsername"`
	LoginPasswordHash string `json:"loginPasswordHash"`
	// SessionSecret signs the login session cookie (HMAC) — generated
	// once, persisted, never shown in the UI. See generateSessionSecret's
	// own comment for why this mirrors APIKey/ClientIdentifier's pattern.
	SessionSecret string `json:"sessionSecret"`
}

// Open loads path if it exists, or starts empty (first run) — either
// way the returned Store owns path from then on. dir is created if
// missing, matching a fresh bind-mounted /data volume.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating data directory: %w", err)
	}
	s := &Store{path: path}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if len(raw) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(raw, &s.data); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return s, nil
}

// writeLocked persists the current state. Caller must hold s.mu for
// writing. Writes to a temp file and renames over the real one, so a
// crash mid-write can never leave a half-written, corrupt store file —
// the rename is atomic on every OS this ships for.
func (s *Store) writeLocked() error {
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding store: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("writing temp store file: %w", err)
	}
	return os.Rename(tmp, s.path)
}

// --- API key ---

// APIKey returns the current key, generating and persisting one on
// first call if none exists yet — see cmd/cliphanger/main.go, which
// calls this once at startup so the key is stable across restarts
// rather than regenerated every time (which would silently lock every
// client out).
func (s *Store) APIKey() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.APIKey != "" {
		return s.data.APIKey, nil
	}
	key, err := generateAPIKey()
	if err != nil {
		return "", err
	}
	s.data.APIKey = key
	return key, s.writeLocked()
}

// ClientIdentifier returns this install's own stable plex.tv client
// identity, generating and persisting one on first call — see the
// field's own comment on diskData.
func (s *Store) ClientIdentifier() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.ClientIdentifier != "" {
		return s.data.ClientIdentifier, nil
	}
	id, err := generateClientIdentifier()
	if err != nil {
		return "", err
	}
	s.data.ClientIdentifier = id
	return id, s.writeLocked()
}

// RetentionHours returns the current retention window in hours — 0
// means forever. See the field's own comment on diskData.
func (s *Store) RetentionHours() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.RetentionHours
}

// SetRetentionHours is the Setup page's "Save" action — always
// overwrites and marks the setting as explicitly configured, so a
// RETENTION_HOURS env var on a later restart no longer applies (same
// reasoning as SeedAPIKeyIfUnset below).
func (s *Store) SetRetentionHours(hours int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.RetentionHours = hours
	s.data.RetentionConfigured = true
	return s.writeLocked()
}

// SeedRetentionHoursIfUnset is RETENTION_HOURS's env-var support
// (cmd/cliphanger/main.go) — only takes effect once, on a store that's
// never had this explicitly configured (by env var OR the web UI).
func (s *Store) SeedRetentionHoursIfUnset(hours int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.RetentionConfigured {
		return nil
	}
	s.data.RetentionHours = hours
	s.data.RetentionConfigured = true
	return s.writeLocked()
}

// DefaultSpanSeconds returns the currently configured default clip
// duration in seconds — model.DefaultSpanSeconds until the Setup page
// has ever been used to change it.
func (s *Store) DefaultSpanSeconds() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.data.DefaultSpanSeconds <= 0 {
		return model.DefaultSpanSeconds
	}
	return s.data.DefaultSpanSeconds
}

// SetDefaultSpanSeconds is the Setup page's clip-duration "Save"
// action — takes effect on the very next job that omits its own
// spanSeconds, no restart needed (queue.process reads this live, same
// pattern as RetentionHours).
func (s *Store) SetDefaultSpanSeconds(seconds int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.DefaultSpanSeconds = seconds
	return s.writeLocked()
}

// DefaultSpeedMultiplier returns the currently configured playback
// speed — model.DefaultSpeedMultiplier (2x) until the Setup page has
// ever been used to change it. See the diskData field's own comment.
func (s *Store) DefaultSpeedMultiplier() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.data.DefaultSpeedMultiplier <= 0 {
		return model.DefaultSpeedMultiplier
	}
	return s.data.DefaultSpeedMultiplier
}

// SetDefaultSpeedMultiplier is the Setup page's playback-speed "Save"
// action — same live-takes-effect pattern as SetDefaultSpanSeconds.
func (s *Store) SetDefaultSpeedMultiplier(multiplier int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.DefaultSpeedMultiplier = multiplier
	return s.writeLocked()
}

// MaxConcurrentJobs returns the current EFFECTIVE concurrency limit —
// the live Setup-page value if one's been explicitly set (manual,
// clamped 1-model.MaxConcurrentJobsCeiling), or an auto-detected value
// otherwise. Read live by the queue on every worker loop iteration
// (queue.Queue.worker), not just once at startup — a Setup-page change
// takes effect on the very next job pull, no restart needed, same
// pattern as RetentionHours/DefaultSpanSeconds.
func (s *Store) MaxConcurrentJobs() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.data.MaxConcurrentJobs <= 0 {
		return AutoMaxConcurrentJobs()
	}
	if s.data.MaxConcurrentJobs > model.MaxConcurrentJobsCeiling {
		return model.MaxConcurrentJobsCeiling
	}
	return s.data.MaxConcurrentJobs
}

// MaxConcurrentJobsIsAuto reports whether the currently effective limit
// came from auto-detection rather than an explicit manual value — used
// only by the Setup page, to show "Auto (currently N)" instead of a
// bare number with no context about where it came from.
func (s *Store) MaxConcurrentJobsIsAuto() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.MaxConcurrentJobs <= 0
}

// SetMaxConcurrentJobs is the Setup page's concurrency "Save" action —
// n <= 0 means Auto. Always overwrites and marks the setting as
// explicitly configured, so a WORKERS env var on a later restart no
// longer applies (same reasoning as SetRetentionHours).
func (s *Store) SetMaxConcurrentJobs(n int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.MaxConcurrentJobs = n
	s.data.MaxConcurrentJobsConfigured = true
	return s.writeLocked()
}

// SeedMaxConcurrentJobsIfUnset is WORKERS's env-var support
// (cmd/cliphanger/main.go) — only takes effect once, on a store that's
// never had this explicitly configured (by env var OR the web UI). A
// fresh install with no WORKERS env var set at all never calls this,
// so it naturally lands on Auto (the zero value) with no extra code.
func (s *Store) SeedMaxConcurrentJobsIfUnset(n int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.MaxConcurrentJobsConfigured {
		return nil
	}
	s.data.MaxConcurrentJobs = n
	s.data.MaxConcurrentJobsConfigured = true
	return s.writeLocked()
}

// AutoMaxConcurrentJobs picks a concurrency limit from the host's
// visible CPU count when nothing's been manually configured. ffmpeg's
// own decode already uses more than one thread internally for most
// codecs, so this deliberately isn't "one worker per core" — that
// would starve each individual job of the threads it wants and likely
// make every job slower, the same failure mode the fixed default of 3
// caused on smaller hardware (see MaxConcurrentJobs's own field
// comment). Conservative in both directions: never below 1 (there's
// always at least one job's worth of work to do), never above
// model.MaxConcurrentJobsCeiling regardless of how many cores are
// visible — a beefy host still shouldn't run more parallel decodes
// than SERVER-NOTES.md's own guidance recommends, since the same box
// is very often ALSO serving media (Plex/Kodi/Jellyfin) at the same
// time. Uses runtime.NumCPU() (logical cores, matching what the Go
// scheduler itself sees) rather than trying to read physical core
// count or clock speed — a portable, dependency-free signal that's
// good enough for a coarse 1-4 decision, not a precision benchmark.
func AutoMaxConcurrentJobs() int {
	cpus := runtime.NumCPU()
	switch {
	case cpus <= 2:
		return 1
	case cpus <= 4:
		return 2
	case cpus <= 8:
		return 3
	default:
		return model.MaxConcurrentJobsCeiling
	}
}

// SeedAPIKeyIfUnset sets the API key to `key` ONLY when the store has
// none yet — cmd/cliphanger/main.go's support for the optional
// API_KEY environment variable (the README's docker-compose example
// sets one explicitly, for infra-as-code setups that want a known key
// rather than an autogenerated one). Deliberately never overwrites an
// EXISTING key: once a key has been generated or seeded once, the env
// var is not re-applied on every restart, or rotating in the web UI
// would be pointless — the next container restart would just put the
// old env-var key back.
func (s *Store) SeedAPIKeyIfUnset(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.APIKey != "" {
		return nil
	}
	s.data.APIKey = key
	return s.writeLocked()
}

// RotateAPIKey replaces the key with a fresh one — the web UI's "Rotate
// API key" action. Every client holding the old key starts getting 401
// immediately; that's the point.
func (s *Store) RotateAPIKey() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, err := generateAPIKey()
	if err != nil {
		return "", err
	}
	s.data.APIKey = key
	return key, s.writeLocked()
}

// --- Local login ---

func (s *Store) LocalLoginEnabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.LocalLoginEnabled
}

func (s *Store) LoginUsername() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.LoginUsername
}

// HasStoredPassword reports whether a password has ever been set — the
// Security page uses this to show "leave blank to keep the current
// password" only once there's actually a current password to keep,
// rather than always implying one exists.
func (s *Store) HasStoredPassword() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.LoginPasswordHash != ""
}

// SetLocalLogin is the Security page's one "Save" action for this whole
// feature — username, an OPTIONAL new password, and the enabled flag,
// together. Password is optional specifically so re-saving just the
// username, or flipping the enabled flag on its own, never forces
// retyping a password that hasn't changed — an empty password here
// means "keep whatever hash is already stored," not "set an empty
// password" (bcrypt would happily hash "" — this deliberately never
// lets that become the actual stored credential).
//
// Enabling requires a username AND a password (new or already-stored)
// to both exist first — refusing otherwise is what keeps this from ever
// locking an admin out the moment they save: without this check, an
// enabled flag with no real credentials behind it would make every
// subsequent request fail a login it's now impossible to complete.
func (s *Store) SetLocalLogin(enabled bool, username, password string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	hasHash := s.data.LoginPasswordHash != ""
	if enabled {
		if username == "" {
			return fmt.Errorf("a username is required to enable login")
		}
		if password == "" && !hasHash {
			return fmt.Errorf("a password is required to enable login")
		}
	}
	s.data.LoginUsername = username
	if password != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return fmt.Errorf("hashing password: %w", err)
		}
		s.data.LoginPasswordHash = string(hash)
	}
	s.data.LocalLoginEnabled = enabled
	return s.writeLocked()
}

// VerifyLogin checks a submitted username/password against the stored
// credentials. bcrypt.CompareHashAndPassword is itself constant-time
// with respect to the password comparison (that's the whole point of
// hashing rather than storing/comparing plaintext); the username
// comparison ahead of it doesn't need the same care since a username is
// not a secret.
func (s *Store) VerifyLogin(username, password string) bool {
	s.mu.RLock()
	hash := s.data.LoginPasswordHash
	wantUser := s.data.LoginUsername
	s.mu.RUnlock()
	if hash == "" || username == "" || username != wantUser {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// SessionSecret returns the key session cookies are signed with,
// generating and persisting one on first call — same pattern as APIKey/
// ClientIdentifier above.
func (s *Store) SessionSecret() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.SessionSecret != "" {
		return s.data.SessionSecret, nil
	}
	secret, err := generateSessionSecret()
	if err != nil {
		return "", err
	}
	s.data.SessionSecret = secret
	return secret, s.writeLocked()
}

// --- Servers ---

func (s *Store) ListServers() []model.Server {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Server, len(s.data.Servers))
	copy(out, s.data.Servers)
	return out
}

func (s *Store) GetServer(id string) (model.Server, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, srv := range s.data.Servers {
		if srv.ID == id {
			return srv, true
		}
	}
	return model.Server{}, false
}

// PutServer inserts (if ID is empty, one is generated) or replaces a
// server by ID.
func (s *Store) PutServer(srv model.Server) (model.Server, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if srv.ID == "" {
		id, err := generateServerID()
		if err != nil {
			return model.Server{}, err
		}
		srv.ID = id
	}
	for i, existing := range s.data.Servers {
		if existing.ID == srv.ID {
			s.data.Servers[i] = srv
			return srv, s.writeLocked()
		}
	}
	s.data.Servers = append(s.data.Servers, srv)
	return srv, s.writeLocked()
}

func (s *Store) DeleteServer(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, srv := range s.data.Servers {
		if srv.ID == id {
			s.data.Servers = append(s.data.Servers[:i], s.data.Servers[i+1:]...)
			return s.writeLocked()
		}
	}
	return nil
}

// SetServerReachability records the result of a "Test connection" (web
// UI) or an opportunistic check made while resolving a real job.
func (s *Store) SetServerReachability(id string, reachable bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, srv := range s.data.Servers {
		if srv.ID == id {
			s.data.Servers[i].LastReachable = reachable
			s.data.Servers[i].LastCheckedAt = time.Now().UTC()
			return s.writeLocked()
		}
	}
	return nil
}

// --- Jobs ---

// SubmitJob is the idempotent insert docs/API.md describes: a job
// already present (matched by CaptureID) is left untouched — and
// reported to the caller as skipped — unless force is true, in which
// case it's reset back to Queued so the worker picks it up again.
// Returns the job's STATE prefix ("queued" for a brand new one) purely
// for the submit response's own `jobs: [{captureId, state}]` shape.
func (s *Store) SubmitJob(j model.Job, force bool) (state model.JobState, accepted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	for i, existing := range s.data.Jobs {
		if existing.CaptureID == j.CaptureID {
			if !force {
				return existing.State, false
			}
			j.CreatedAt = existing.CreatedAt
			j.State = model.StateQueued
			j.UpdatedAt = now
			s.data.Jobs[i] = j
			_ = s.writeLocked()
			return j.State, true
		}
	}
	j.State = model.StateQueued
	j.CreatedAt = now
	j.UpdatedAt = now
	s.data.Jobs = append(s.data.Jobs, j)
	_ = s.writeLocked()
	return j.State, true
}

// ListJobs returns every job, newest-updated first, optionally filtered
// by state and/or only those updated after `since`.
func (s *Store) ListJobs(stateFilter model.JobState, since time.Time) []model.Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Job, 0, len(s.data.Jobs))
	for _, j := range s.data.Jobs {
		if stateFilter != "" && j.State != stateFilter {
			continue
		}
		if !since.IsZero() && !j.UpdatedAt.After(since) {
			continue
		}
		out = append(out, j)
	}
	return out
}

func (s *Store) GetJob(captureID string) (model.Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, j := range s.data.Jobs {
		if j.CaptureID == captureID {
			return j, true
		}
	}
	return model.Job{}, false
}

// NextQueued returns one job in `queued` state and atomically marks it
// `running`, or false if the queue is empty — the worker pool's own
// polling primitive (see internal/queue). Doing the state flip inside
// the same locked section as the scan is what makes two workers unable
// to grab the same job.
func (s *Store) NextQueued() (model.Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, j := range s.data.Jobs {
		if j.State == model.StateQueued {
			now := time.Now().UTC()
			s.data.Jobs[i].State = model.StateRunning
			s.data.Jobs[i].UpdatedAt = now
			// StartedAt (2026-08-24) — the one moment this is ever set.
			// UpdatedAt gets overwritten again the moment the job
			// finishes (UpdateJob), which is exactly why Job.Duration
			// needs THIS field rather than trying to recover "when did
			// running actually start" from a value that's already moved
			// on by the time anything reads it.
			s.data.Jobs[i].StartedAt = &now
			_ = s.writeLocked()
			return s.data.Jobs[i], true
		}
	}
	return model.Job{}, false
}

// UpdateJob overwrites a job by CaptureID (the worker reporting a
// finished attempt, success or failure) and stamps UpdatedAt.
func (s *Store) UpdateJob(j model.Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j.UpdatedAt = time.Now().UTC()
	for i, existing := range s.data.Jobs {
		if existing.CaptureID == j.CaptureID {
			j.CreatedAt = existing.CreatedAt
			s.data.Jobs[i] = j
			return s.writeLocked()
		}
	}
	return fmt.Errorf("job %q not found", j.CaptureID)
}

// ReapTerminalOlderThan deletes every Done or Failed job whose
// UpdatedAt is before cutoff and returns the deleted jobs, so the
// caller (package queue) can also remove their media files off disk —
// this method only ever touches the JSON record. Queued and Running
// jobs are never reaped regardless of age: "old" for a job still in
// flight just means slow, not expired (see queue.DefaultJobTimeout for
// the actual stuck-job cutoff, a separate concern from retention).
func (s *Store) ReapTerminalOlderThan(cutoff time.Time) []model.Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	var reaped []model.Job
	kept := make([]model.Job, 0, len(s.data.Jobs))
	for _, j := range s.data.Jobs {
		terminal := j.State == model.StateDone || j.State == model.StateFailed
		if terminal && j.UpdatedAt.Before(cutoff) {
			reaped = append(reaped, j)
			continue
		}
		kept = append(kept, j)
	}
	if len(reaped) == 0 {
		return nil
	}
	s.data.Jobs = kept
	_ = s.writeLocked()
	return reaped
}

func (s *Store) DeleteJob(captureID string) (model.Job, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, j := range s.data.Jobs {
		if j.CaptureID == captureID {
			s.data.Jobs = append(s.data.Jobs[:i], s.data.Jobs[i+1:]...)
			return j, true, s.writeLocked()
		}
	}
	return model.Job{}, false, nil
}

// Counts returns queue depth by state, for GET /health.
func (s *Store) Counts() (queued, running, failed int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, j := range s.data.Jobs {
		switch j.State {
		case model.StateQueued:
			queued++
		case model.StateRunning:
			running++
		case model.StateFailed:
			failed++
		}
	}
	return
}
