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
	"sync"
	"time"

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
			s.data.Jobs[i].State = model.StateRunning
			s.data.Jobs[i].UpdatedAt = time.Now().UTC()
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
