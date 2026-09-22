// Package model holds the shapes shared across the whole service — the
// job/server/media-info types that both the client-facing API (package
// api) and the web UI (package web) read and write. Field names and
// JSON tags follow docs/API.md exactly; that document is the source of
// truth, not this file — if they drift, fix here to match there.
package model

import (
	"fmt"
	"time"
)

// ServerKind is one of the backends ClipHanger talks to. No backend is
// privileged (CLAUDE.md's "no privileged backend" rule) — this type
// exists purely to pick which Backend implementation resolves a given
// Source, never to branch UI or API behavior by kind.
type ServerKind string

const (
	KindPlex     ServerKind = "plex"
	KindKodi     ServerKind = "kodi"
	KindJellyfin ServerKind = "jellyfin"
	// KindLocal added 2026-09-12, per direct request: reads a file
	// straight off a disk ClipHanger already has access to (a NAS share
	// mounted into its own container, say) instead of streaming it from
	// a media server over HTTP — no server, no credentials, nothing
	// network-facing. See internal/backend/local.go. A deliberate,
	// explicitly-configured server the user sets up FOR this purpose —
	// not the same thing as Kodi's own optional LocalPathFrom/LocalPathTo
	// fallback (still there, unchanged, narrower: only engages after a
	// CONFIRMED 401 on Kodi's own VFS). Reuses those same two fields for
	// its own mapping, since a KindLocal server has nothing else to
	// configure.
	KindLocal ServerKind = "local"
)

// Server is one configured media server. Credentials live here but are
// NEVER serialized into the client-facing API — see
// Server.Public()/PublicServer below, which is the only shape that
// crosses the wire to a client. This type itself is only ever
// marshaled to the on-disk store and the (API-key-gated) web UI.
type Server struct {
	ID   string     `json:"serverId"`
	Kind ServerKind `json:"kind"`
	Name string     `json:"name"`
	Host string     `json:"host"`
	Port int        `json:"port"`

	// Exactly one of these is set, matching Kind. Kept as separate
	// fields rather than a single opaque blob so the web UI's edit form
	// has real fields to bind to, and so a future `go vet`-style
	// grep for "token" or "password" actually finds them.
	PlexToken     string `json:"plexToken,omitempty"`
	KodiUsername  string `json:"kodiUsername,omitempty"`
	KodiPassword  string `json:"kodiPassword,omitempty"`
	JellyfinToken string `json:"jellyfinToken,omitempty"`

	// LocalPathFrom/LocalPathTo are an OPTIONAL prefix mapping — the
	// same "remote path mapping" pattern *arr-stack tools use, since a
	// media server's own view of a file's path and ClipHanger's view of
	// that same file (if it happens to also have access, e.g. the same
	// NAS share mounted separately) are usually rooted differently.
	// Empty (the default) means no mapping is configured, and nothing
	// about this server's behavior changes — HTTP streaming from the
	// server remains the only path used. Added 2026-08-22, scoped
	// specifically as a fallback for Kodi's documented "VFS refuses
	// paths outside a configured source" failure (401, not 404) — see
	// internal/backend/kodi.go and docs/DECISIONS.md. This is NOT a
	// general "skip the server, read files directly" mode: it only ever
	// engages when the server's own HTTP path has been confirmed not to
	// work for a specific item.
	LocalPathFrom string `json:"localPathFrom,omitempty"`
	LocalPathTo   string `json:"localPathTo,omitempty"`
	// LastAttemptedPath is the raw path a KindLocal server's own
	// LocalBackend.Resolve most recently saw and couldn't map — the
	// literal itemID a client submitted, before any prefix check.
	// Added 2026-09-13 (real report: setting LocalPathFrom by typing it
	// blind was "still puzzling... how do I know [it]?") specifically so
	// the Setup page can show it and offer to adopt it as LocalPathFrom
	// in one click, instead of asking someone to dig a failed job's
	// error out of the Jobs page by hand. Meaningless for every other
	// Kind — nothing sets it for them.
	LastAttemptedPath string `json:"lastAttemptedPath,omitempty"`

	// Set by the "Test connection" button in the web UI and refreshed
	// opportunistically whenever a job resolves against this server.
	// Best-effort, not live — see PublicServer.Reachable's own comment.
	LastReachable bool      `json:"lastReachable"`
	LastCheckedAt time.Time `json:"lastCheckedAt"`
}

// PublicServer is the ONLY shape of a Server a client ever sees — GET
// /servers is explicitly "read-only and credential-free" (API.md). No
// credential field of Server has a JSON tag that would survive
// marshaling this instead of Server, but the real safety is that
// nothing in package api ever marshals a Server directly — it always
// converts through this first.
type PublicServer struct {
	ID        string     `json:"serverId"`
	Kind      ServerKind `json:"kind"`
	Name      string     `json:"name"`
	Reachable bool       `json:"reachable"`
}

func (s Server) Public() PublicServer {
	return PublicServer{ID: s.ID, Kind: s.Kind, Name: s.Name, Reachable: s.LastReachable}
}

// JobState is one of the four states a Job moves through, always in
// this order: never backwards, never skipping Running even for an
// instant capture (the client-visible transition matters more than the
// state actually being observed).
type JobState string

const (
	StateQueued  JobState = "queued"
	StateRunning JobState = "running"
	StateDone    JobState = "done"
	StateFailed  JobState = "failed"
)

// Source names WHERE a capture comes from — a server plus that server's
// OWN id for the item. Never a Plex ratingKey shape leaking into a
// Kodi/Jellyfin job; see CLAUDE.md's "no privileged backend" rule.
type Source struct {
	ServerID string `json:"serverId"`
	ItemID   string `json:"itemId"`
}

// Job is one capture request end to end. CaptureID is chosen by the
// CLIENT and is ClipHanger's idempotency key — see Store.SubmitJob.
type Job struct {
	CaptureID        string `json:"captureId"`
	Source           Source `json:"source"`
	TimestampSeconds int    `json:"timestampSeconds"`
	SpanSeconds      int    `json:"spanSeconds"`
	FPS              int    `json:"fps"`
	// SpeedMultiplier plays back MORE of the source in the same output
	// duration (2026-08-24, per direct request — "a scene can be a few
	// minutes long... [a straight 20s clip] won't actually have the
	// full scene's context"). At 2x, a 20s clip covers 40s of source,
	// time-compressed via ffmpeg's setpts filter — see extract.Clip.
	// Setup-page-only for now (no DemoFlex-side per-request control),
	// so this is effectively always 0/omitted on real submissions,
	// falling back to Store.DefaultSpeedMultiplier().
	SpeedMultiplier int `json:"speedMultiplier,omitempty"`
	// ResolvedSource is the actual file/URL a backend resolved
	// Source.ItemID to, ALREADY REDACTED of any credential
	// (backend.ResolvedSource.Redacted()) — set once Resolve succeeds,
	// so it's there for the Jobs list/detail regardless of whether the
	// job goes on to succeed or fail at the ffmpeg step (2026-09-11,
	// real report: "the jobs list doesnt show which movie it was or
	// which file it was used... just shows a uid and some generation
	// setup details"). This is package model's own domain data (the
	// file a job actually read), not something a client told it —
	// still true, and still the reason this package has no broader
	// concept of client-supplied metadata (CLAUDE.md: "a reader of this
	// repo should never need to know" which client submitted a job).
	// Empty until Resolve runs (still queued, or the server/item itself
	// couldn't be found at all).
	ResolvedSource string `json:"resolvedSource,omitempty"`
	// Title is the item's own display title, straight from the media
	// server (2026-09-22, real report: a raw internal file path "wont
	// make any sense to the user"). Distinct from the "not a movie
	// title" reasoning ResolvedSource's own comment still stands
	// behind: this is the SERVER's title, not anything DemoFlex (or any
	// other client) supplied, fetched from the exact same server call
	// that already resolves the file — see backend.ResolvedSource.Title.
	// Best-effort: empty when a backend couldn't get one (Jellyfin's
	// own lookup failing, say), never a reason to fail the job.
	Title          string     `json:"title,omitempty"`
	State          JobState   `json:"state"`
	Error          string     `json:"error,omitempty"`
	FrameCount     int        `json:"frameCount,omitempty"`
	StillBytes     int        `json:"stillBytes,omitempty"`
	ClipBytes      int        `json:"clipBytes,omitempty"`
	MediaInfo      *MediaInfo `json:"mediaInfo,omitempty"`
	UpdatedAt      time.Time  `json:"updatedAt"`
	CreatedAt      time.Time  `json:"createdAt"`
	// StartedAt is when a WORKER actually picked this job up (stamped by
	// Store.NextQueued, the queued→running transition) — distinct from
	// CreatedAt, which is when the client submitted it and can sit
	// behind other jobs for a while if WORKERS is busy. Nil until that
	// happens. Added 2026-08-24, per direct request ("show the total
	// time took to generate the clip in the job detail") — see Duration
	// below, the whole reason this exists.
	StartedAt *time.Time `json:"startedAt,omitempty"`
}

// Duration reports how long the actual extraction took, formatted for
// display — CreatedAt→UpdatedAt was the OTHER, simpler way to compute
// this from fields that already existed, but deliberately not used:
// that span also counts time spent waiting behind other jobs for a free
// worker, a WORKERS-concurrency artifact having nothing to do with how
// long the clip itself took to generate, and would read as misleading
// the moment more than one job is queued at once. Empty whenever
// there's nothing to report yet — StartedAt unset (still queued), or
// the job hasn't reached a terminal state.
func (j Job) Duration() string {
	if j.StartedAt == nil || (j.State != StateDone && j.State != StateFailed) {
		return ""
	}
	d := j.UpdatedAt.Sub(*j.StartedAt)
	if d < 0 {
		return ""
	}
	if d < time.Minute {
		return fmt.Sprintf("%.0fs", d.Seconds())
	}
	return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
}

// Defaults, applied when a submitted job omits them — see
// docs/DECISIONS.md "Output is fixed, not negotiated": spanSeconds and
// fps are the only two knobs a client gets at all.
//
// DefaultSpanSeconds was 3 until 2026-08-23 — a leftover from before the
// clip's actual intended length was decided (a real user report: "make
// sure the clips are actually showing 20 seconds... feels like a few
// seconds", tracing back to a 3s job that never got the later 20s
// default threaded through this constant). 20s matches the client-side
// default DemoFlex itself now also sends explicitly on every submission
// (see ClipHangerMediaClient.swift) — this constant only still matters
// as the fallback for any OTHER client that omits spanSeconds entirely.
const (
	DefaultSpanSeconds = 20
	DefaultFPS         = 10
	// DefaultSpeedMultiplier: 2x by direct decision — a plain 1x/20s
	// clip only ever shows 20 real seconds of a scene that can run
	// several minutes, missing most of its context. 2x doubles how much
	// of the scene a clip actually covers for the same output length.
	DefaultSpeedMultiplier = 2
	// MaxConcurrentJobsCeiling is the hard cap on simultaneous ffmpeg
	// processes, manual or auto-detected — see docs/SERVER-NOTES.md's
	// "cap parallel ffmpeg processes at 2-4" guidance: each is a full
	// decode, and the same box often serves media at the same time, so
	// throughput doesn't keep scaling with worker count past this point
	// — it just means each job gets a smaller slice of the same CPU and
	// finishes slower. Shared by Store.MaxConcurrentJobs (manual clamp)
	// and Store.AutoMaxConcurrentJobs (auto-detected ceiling).
	MaxConcurrentJobsCeiling = 4
)

// MediaInfo is ffprobe's findings about the source file, attached to a
// job once it's Done — see docs/SERVER-NOTES.md for the derivation
// rules this is built from (HDR/Atmos/DTS:X detection, the width-not-
// height 4K test, etc.).
type MediaInfo struct {
	DurationMs int64       `json:"durationMs"`
	Container  string      `json:"container"`
	Video      VideoInfo   `json:"video"`
	Audio      []AudioInfo `json:"audio"`
}

type VideoInfo struct {
	Codec       string       `json:"codec"`
	Width       int          `json:"width"`
	Height      int          `json:"height"`
	BitDepth    int          `json:"bitDepth,omitempty"`
	FrameRate   string       `json:"frameRate,omitempty"`
	HDR         string       `json:"hdr,omitempty"` // "HDR10" | "HLG" | ""
	DolbyVision *DolbyVision `json:"dolbyVision,omitempty"`
}

type DolbyVision struct {
	Profile int `json:"profile"`
	Level   int `json:"level"`
}

type AudioInfo struct {
	Codec         string `json:"codec"`
	Profile       string `json:"profile,omitempty"`
	Channels      int    `json:"channels"`
	ChannelLayout string `json:"channelLayout,omitempty"`
	Spatial       string `json:"spatial,omitempty"` // "atmos" | "dtsx" | ""
	Default       bool   `json:"default"`
}
