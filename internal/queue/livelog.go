package queue

import (
	"sync"
)

// liveLogRegistry holds an in-memory, EPHEMERAL ring buffer of ffmpeg's
// own real-time output per currently-running job (2026-08-22, per
// direct request — "we need to show the execution live log also
// somewhere if user wants to see"). Deliberately not persisted: this is
// for watching a job that's ACTIVELY running right now (is it stuck, or
// just slow?), not a permanent record — a failed job's own tail is
// already captured into Job.Error, which IS persisted; this is the
// thing to check WHILE waiting, before that final tail even exists.
type liveLogRegistry struct {
	mu   sync.RWMutex
	logs map[string]*ringLog
}

func newLiveLogRegistry() *liveLogRegistry {
	return &liveLogRegistry{logs: make(map[string]*ringLog)}
}

// start registers a fresh log for captureID, replacing any stale one —
// relevant if a previous attempt at the same captureId was force-
// resubmitted. Returns the writer to hand to extract.Still/Clip/Probe.
func (r *liveLogRegistry) start(captureID string) *ringLog {
	log := newRingLog()
	r.mu.Lock()
	r.logs[captureID] = log
	r.mu.Unlock()
	return log
}

// finish removes captureID's log once the job is done — see the type's
// own comment on why this is ephemeral, not kept around after the fact.
func (r *liveLogRegistry) finish(captureID string) {
	r.mu.Lock()
	delete(r.logs, captureID)
	r.mu.Unlock()
}

// Read returns the current buffered text for captureID, or false if
// nothing is running under that id right now (never ran, already
// finished, or the id doesn't exist at all — this can't distinguish
// those, which is fine: the caller's answer to the user is the same
// either way, "no live log right now").
func (r *liveLogRegistry) Read(captureID string) (string, bool) {
	r.mu.RLock()
	log, ok := r.logs[captureID]
	r.mu.RUnlock()
	if !ok {
		return "", false
	}
	return log.String(), true
}

// ringLog is an io.Writer that keeps only the last maxLines lines —
// ffmpeg's own chatter for even a few seconds of a high-bitrate source
// can run to hundreds of lines (per-frame stats at some log levels), and
// nobody's watching this for a scrollback, just "is it moving."
type ringLog struct {
	mu    sync.Mutex
	lines []string
	cur   []byte
}

const maxLiveLogLines = 300

func newRingLog() *ringLog {
	return &ringLog{lines: make([]string, 0, maxLiveLogLines)}
}

func (l *ringLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cur = append(l.cur, p...)
	for {
		i := indexByte(l.cur, '\n')
		if i < 0 {
			break
		}
		l.appendLineLocked(string(l.cur[:i]))
		l.cur = l.cur[i+1:]
	}
	return len(p), nil
}

func (l *ringLog) appendLineLocked(line string) {
	l.lines = append(l.lines, line)
	if len(l.lines) > maxLiveLogLines {
		l.lines = l.lines[len(l.lines)-maxLiveLogLines:]
	}
}

func (l *ringLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.lines)+1)
	out = append(out, l.lines...)
	if len(l.cur) > 0 {
		out = append(out, string(l.cur)) // whatever's been written since the last newline
	}
	return joinLines(out)
}

func indexByte(b []byte, c byte) int {
	for i, v := range b {
		if v == c {
			return i
		}
	}
	return -1
}

func joinLines(lines []string) string {
	total := 0
	for _, s := range lines {
		total += len(s) + 1
	}
	buf := make([]byte, 0, total)
	for _, s := range lines {
		buf = append(buf, s...)
		buf = append(buf, '\n')
	}
	return string(buf)
}
