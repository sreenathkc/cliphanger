// Package queue is the worker pool that turns a Queued job into a
// Done or Failed one: resolve the source, run ffmpeg twice (still +
// clip), probe once, write results, update the store. Nothing in here
// is client-facing — package api is the only thing that talks to a
// client; this package only ever talks to the store and to ffmpeg.
package queue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/sreenathkc/cliphanger/internal/backend"
	"github.com/sreenathkc/cliphanger/internal/extract"
	"github.com/sreenathkc/cliphanger/internal/model"
	"github.com/sreenathkc/cliphanger/internal/store"
)

// DefaultJobTimeout bounds one job's whole resolve+extract+probe
// pipeline — a stuck ffmpeg process (a server that accepts the
// connection but never sends data, say) would otherwise pin a worker
// slot forever. Configurable (JOB_TIMEOUT_SECONDS env var, see
// cmd/cliphanger/main.go) because how long is reasonable depends on
// the network between ClipHanger and the media server — confirmed via
// a real report: MKV streamed over a home LAN can take longer than 5
// minutes to seek+encode a single short clip, since Matroska's own seek
// index isn't always positioned as conveniently for a remote byte-range
// seek as MP4's is.
const DefaultJobTimeout = 5 * time.Minute

// DefaultRetention is a SUGGESTED value shown as placeholder text on
// the Setup page's retention field — not the actual default anymore.
// Retention started as an open question in docs/DECISIONS.md ("does
// ClipHanger keep media forever or expire it?"), was first resolved as
// a bounded 72h cache, then changed again 2026-08-22 per direct
// request ("by default forever, user can change in settings") — the
// real default is now 0 (forever), read live from the store
// (Store.RetentionHours), changeable anytime from the Setup page
// without a restart. 72h remains a reasonable number to suggest to
// someone who DOES want a bound: enough to "get to it this weekend"
// without a self-hosted box's disk growing unbounded from jobs nobody
// ever collected.
const DefaultRetention = 72 * time.Hour

// reapInterval is how often the retention sweep runs — coarse on
// purpose, this is background housekeeping, not something anyone
// watches in real time (that's what the live log is for).
const reapInterval = 15 * time.Minute

type Queue struct {
	store      *store.Store
	registry   *backend.Registry
	mediaDir   string
	jobTimeout time.Duration
	logger     *slog.Logger
	liveLogs   *liveLogRegistry
	// running is how many jobs are actively executing right now —
	// checked against the LIVE store.MaxConcurrentJobs() limit before a
	// worker claims new work (see worker's own comment). Not the same
	// thing as goroutine count any more: model.MaxConcurrentJobsCeiling
	// worker goroutines are always running (see Run), but only up to
	// the live limit of them are ever doing real work at once.
	running atomic.Int32
}

// New — concurrency is NOT a parameter here (2026-08-26, replacing the
// old fixed numWorkers) — see Store.MaxConcurrentJobs's own comment for
// why a value fixed once at startup, with no hardware awareness, was
// exactly what produced a real slowdown report. It's read LIVE from the
// store on every worker loop iteration instead, same pattern retention
// already used (reapOnce) — a Setup-page change takes effect on the
// very next job pull, no restart needed. mediaDir is where generated
// stills/clips are written; caller (cmd/cliphanger/main.go) is
// responsible for it existing. jobTimeout <= 0 falls back to
// DefaultJobTimeout.
func New(st *store.Store, registry *backend.Registry, mediaDir string, jobTimeout time.Duration, logger *slog.Logger) *Queue {
	if jobTimeout <= 0 {
		jobTimeout = DefaultJobTimeout
	}
	return &Queue{store: st, registry: registry, mediaDir: mediaDir, jobTimeout: jobTimeout, logger: logger, liveLogs: newLiveLogRegistry()}
}

// Run blocks until ctx is cancelled. Always starts
// model.MaxConcurrentJobsCeiling polling goroutines — the most jobs
// that could EVER be allowed to run at once — but each one gates
// itself against the LIVE store.MaxConcurrentJobs() limit before
// claiming work (see worker's own comment), so the number actually
// running in parallel at any moment can be anywhere from 1 up to that
// ceiling and can change at any time via the Setup page. This is
// simpler and race-safer than trying to grow/shrink the goroutine pool
// itself to match a changing live value.
func (q *Queue) Run(ctx context.Context) {
	done := make(chan struct{})
	for i := 0; i < model.MaxConcurrentJobsCeiling; i++ {
		go q.worker(ctx, i, done)
	}
	// Always runs, unlike the old fixed-at-startup version — reapOnce
	// itself is a no-op whenever the live retention setting is 0
	// (forever), so there's nothing to gate here anymore; the setting
	// can change at any time via the Setup page.
	go q.reapLoop(ctx)
	<-ctx.Done()
	for i := 0; i < model.MaxConcurrentJobsCeiling; i++ {
		<-done
	}
}

// reapLoop periodically deletes Done/Failed jobs (and their media)
// older than the live retention setting. Sweeps once immediately, not
// just on the first tick, so a box that was off for a while (or just
// restarted) doesn't wait a full reapInterval before catching up on
// jobs that expired while it was down.
func (q *Queue) reapLoop(ctx context.Context) {
	q.reapOnce()
	ticker := time.NewTicker(reapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			q.reapOnce()
		}
	}
}

func (q *Queue) reapOnce() {
	hours := q.store.RetentionHours()
	if hours <= 0 {
		return // forever — nothing to sweep
	}
	retention := time.Duration(hours) * time.Hour
	cutoff := time.Now().UTC().Add(-retention)
	reaped := q.store.ReapTerminalOlderThan(cutoff)
	for _, j := range reaped {
		q.DeleteMedia(j.CaptureID)
	}
	if len(reaped) > 0 {
		q.logger.Info("retention sweep reaped expired jobs", "count", len(reaped), "retentionHours", hours)
	}
}

func (q *Queue) worker(ctx context.Context, id int, done chan<- struct{}) {
	defer func() { done <- struct{}{} }()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Claim a running-slot optimistically, then check whether that
		// put it over the LIVE limit — not "check then increment",
		// which would let two workers both see room for one more job
		// and both proceed (a genuine race: q.running.Load() and the
		// eventual q.running.Add(1) aren't one atomic operation).
		// Increment-then-verify is race-safe: worst case, two workers
		// both increment past the limit at once and BOTH back off this
		// cycle even though one slot was really free — a wasted second
		// of idle, self-correcting on the very next loop iteration.
		// That's a far better trade for a soft performance guideline
		// than silently running more parallel ffmpeg decodes than
		// configured, which is the actual bug being fixed here.
		if int(q.running.Add(1)) > q.store.MaxConcurrentJobs() {
			q.running.Add(-1)
			select {
			case <-ctx.Done():
				return
			case <-time.After(1 * time.Second):
			}
			continue
		}

		job, ok := q.store.NextQueued()
		if !ok {
			q.running.Add(-1)
			// Nothing to do — a fixed short poll interval is plenty for
			// a self-hosted tool serving a handful of clients; no
			// pub/sub needed for this scale.
			select {
			case <-ctx.Done():
				return
			case <-time.After(1 * time.Second):
			}
			continue
		}

		jobCtx, cancel := context.WithTimeout(ctx, q.jobTimeout)
		q.process(jobCtx, job)
		cancel()
		q.running.Add(-1)
	}
}

// StillPath/ClipPath are keyed on a hash of CaptureID, not the raw
// string — "captureId is client-chosen and may contain anything"
// (docs/API.md), which makes it unsafe to use directly as a filename
// (path separators, length limits, reserved characters all vary by
// filesystem). The mapping only ever needs to be looked up by
// CaptureID again, never listed on disk by a human, so a hash loses
// nothing.
func (q *Queue) mediaPath(captureID, ext string) string {
	sum := sha256.Sum256([]byte(captureID))
	return filepath.Join(q.mediaDir, hex.EncodeToString(sum[:])+"."+ext)
}

func (q *Queue) process(ctx context.Context, job model.Job) {
	sink := q.liveLogs.start(job.CaptureID)
	defer q.liveLogs.finish(job.CaptureID)

	fail := func(err error) {
		job.State = model.StateFailed
		job.Error = err.Error()
		if updateErr := q.store.UpdateJob(job); updateErr != nil {
			q.logger.Error("failed to persist failed job", "captureId", job.CaptureID, "error", updateErr)
		}
		q.logger.Warn("job failed", "captureId", job.CaptureID, "error", err)
	}

	server, ok := q.store.GetServer(job.Source.ServerID)
	if !ok {
		// Wording covers both real causes evenly (2026-08-26) — this
		// used to only mention "removed after submission," but package
		// api no longer rejects an unknown serverId before a job is
		// even created (see handleSubmitJobs's own comment), so "never
		// configured in the first place" is now the more common case
		// reaching here, not the rarer one.
		fail(fmt.Errorf("server %q is not configured — add it on the Setup page (it may never have been added, or was removed after this job was submitted)", job.Source.ServerID))
		return
	}

	be, err := q.registry.For(server.Kind)
	if err != nil {
		fail(err)
		return
	}

	resolved, err := be.Resolve(ctx, server, job.Source.ItemID)
	if err != nil {
		_ = q.store.SetServerReachability(server.ID, false)
		fail(fmt.Errorf("resolving source: %w", err))
		return
	}
	_ = q.store.SetServerReachability(server.ID, true)

	src := extract.Source{
		URL:            resolved.URL,
		RedactedURL:    resolved.Redacted(),
		ExtraInputArgs: resolved.ExtraInputArgs,
		LogSink:        sink,
	}

	// q.store.DefaultSpanSeconds(), not the bare model.DefaultSpanSeconds
	// constant (2026-08-23) — the Setup page's own live-editable clip-
	// duration setting; reads live off the store on every job the same
	// way retention does, so a change there takes effect immediately,
	// no restart.
	span := job.SpanSeconds
	if span <= 0 {
		span = q.store.DefaultSpanSeconds()
	}
	fps := job.FPS
	if fps <= 0 {
		fps = model.DefaultFPS
	}
	// Same live-store pattern as span above (2026-08-24) — see
	// Store.DefaultSpeedMultiplier's own comment.
	speedMultiplier := job.SpeedMultiplier
	if speedMultiplier <= 0 {
		speedMultiplier = q.store.DefaultSpeedMultiplier()
	}

	stillPath := q.mediaPath(job.CaptureID, "jpg")
	clipPath := q.mediaPath(job.CaptureID, "mp4")

	if err := extract.Still(ctx, src, job.TimestampSeconds, stillPath); err != nil {
		fail(fmt.Errorf("still: %w", err))
		return
	}
	stillInfo, _ := os.Stat(stillPath)

	clipResult, err := extract.Clip(ctx, src, job.TimestampSeconds, span, fps, speedMultiplier, clipPath)
	if err != nil {
		fail(fmt.Errorf("clip: %w", err))
		return
	}
	// A capture yielding one frame is a FAILURE, not a success — the
	// caller asked for motion (CLAUDE.md, docs/API.md). This exact
	// class of bug hid for a whole round of testing during design by
	// being reported as "done."
	if clipResult.FrameCount <= 1 {
		_ = os.Remove(clipPath)
		_ = os.Remove(stillPath)
		fail(fmt.Errorf("clip produced only %d frame(s) — nothing to animate. The source may be shorter than expected at this timestamp, or the server returned a static/black segment", clipResult.FrameCount))
		return
	}

	mediaInfo, err := extract.Probe(ctx, src)
	if err != nil {
		// Probing is supplementary, not the point of the job — a still
		// and a real clip already exist at this point. Log and move on
		// rather than failing a job that otherwise succeeded.
		q.logger.Warn("probe failed, continuing without mediaInfo", "captureId", job.CaptureID, "error", err)
		mediaInfo = nil
	}

	job.State = model.StateDone
	job.Error = ""
	job.FrameCount = clipResult.FrameCount
	job.ClipBytes = clipResult.Bytes
	if stillInfo != nil {
		job.StillBytes = int(stillInfo.Size())
	}
	job.MediaInfo = mediaInfo

	if err := q.store.UpdateJob(job); err != nil {
		q.logger.Error("failed to persist done job", "captureId", job.CaptureID, "error", err)
		return
	}
	q.logger.Info("job done", "captureId", job.CaptureID, "frames", clipResult.FrameCount, "clipBytes", clipResult.Bytes)
}

// LiveLog returns the buffered live ffmpeg output for a job that's
// actively running right now, or false if there's nothing to show
// (queued, done, failed, or an unrecognized captureID — package web's
// handler treats all of those the same: "no live log right now").
func (q *Queue) LiveLog(captureID string) (string, bool) {
	return q.liveLogs.Read(captureID)
}

// MediaPaths exposes the same hashed paths for package api's media
// handlers — StillPath/ClipPath, not mediaPath, since those need to be
// reachable from outside this package.
func (q *Queue) StillPath(captureID string) string { return q.mediaPath(captureID, "jpg") }
func (q *Queue) ClipPath(captureID string) string  { return q.mediaPath(captureID, "mp4") }

// DeleteMedia removes a job's generated files — the DELETE /jobs/{id}
// escape hatch. Missing files are not an error (the job may have failed
// before either was written).
func (q *Queue) DeleteMedia(captureID string) {
	_ = os.Remove(q.StillPath(captureID))
	_ = os.Remove(q.ClipPath(captureID))
}
