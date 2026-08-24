// Package backend is the ONLY part of ClipHanger that knows which
// media server it's talking to (CLAUDE.md: "this is the only part of
// ClipHanger that knows which server it's talking to. Everything
// downstream sees a URL."). Each Backend turns a (Server, itemID) pair
// into something ffmpeg can read directly; nothing above this package —
// the queue, the extractor, the API — ever branches on ServerKind.
package backend

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/sreenathkc/cliphanger/internal/model"
)

// ResolvedSource is everything ffmpeg needs to read one item from one
// server.
type ResolvedSource struct {
	// URL ffmpeg reads via -i. May itself carry a credential (Plex's
	// token as a query param, Kodi's basic auth in the userinfo
	// component) — that's fine, ffmpeg supports both; what must NEVER
	// happen is this exact value reaching a log line or an error
	// string. Use Redacted() for that.
	URL string
	// Extra arguments inserted BEFORE -i (see docs/SERVER-NOTES.md:
	// "the credential is a header... must be passed to ffmpeg/ffprobe
	// with -headers placed before -i"). Empty for Plex/Kodi, which
	// carry their credential in URL itself; Jellyfin populates this.
	ExtraInputArgs []string
}

// Redacted returns URL with any credential-shaped query parameter or
// userinfo component replaced, safe to place in a log line or a job's
// Error field. Belt-and-braces alongside each backend already
// constructing URLs carefully — this is the LAST line of defense
// CLAUDE.md's "never let a credential reach a log or an error string"
// rule asks for, applied once, centrally, rather than trusted to be
// remembered at every call site that might log a URL.
func (r ResolvedSource) Redacted() string {
	return redact(r.URL)
}

var credentialParamPattern = regexp.MustCompile(`(?i)([?&](?:X-Plex-Token|api_key|apikey|token)=)[^&]+`)
var userinfoPattern = regexp.MustCompile(`://[^/@]+@`)

func redact(url string) string {
	url = credentialParamPattern.ReplaceAllString(url, "$1REDACTED")
	url = userinfoPattern.ReplaceAllString(url, "://REDACTED@")
	return url
}

// Backend resolves items for exactly one ServerKind and can verify a
// Server's credentials actually work.
type Backend interface {
	Kind() model.ServerKind
	Resolve(ctx context.Context, server model.Server, itemID string) (ResolvedSource, error)
	TestConnection(ctx context.Context, server model.Server) error
}

// LocalPath translates rawPath — the file path a media server ITSELF
// reports for an item (Plex's Part.file, Kodi's movie `file` property,
// Jellyfin's item Path) — into a path ClipHanger can read directly,
// using the server's optional LocalPathFrom/LocalPathTo prefix mapping.
// Added 2026-08-22 as a scoped fallback for cases where a server's own
// HTTP serving is confirmed not to work for a specific item (the
// motivating case: Kodi's VFS refusing anything outside a configured
// source — see kodi.go). Returns ("", false) when no mapping is
// configured, or rawPath doesn't start with LocalPathFrom — callers
// treat that as "no fallback available here," not an error, since an
// unconfigured mapping is the normal/default state for every server.
func LocalPath(server model.Server, rawPath string) (string, bool) {
	if server.LocalPathFrom == "" || server.LocalPathTo == "" {
		return "", false
	}
	if !strings.HasPrefix(rawPath, server.LocalPathFrom) {
		return "", false
	}
	rest := strings.TrimPrefix(rawPath, server.LocalPathFrom)
	return filepath.Join(server.LocalPathTo, rest), true
}

// httpTimeout bounds every backend's own metadata-lookup calls (NOT the
// eventual ffmpeg stream, which is a separate, longer-lived read) — a
// media server that's down should fail a job in seconds, not hang the
// worker that picked it up.
const httpTimeout = 10 * time.Second

func newHTTPClient() *http.Client {
	return &http.Client{Timeout: httpTimeout}
}

// registry maps ServerKind to the Backend that handles it — built once
// in cmd/cliphanger/main.go and threaded through the queue.
type Registry struct {
	backends map[model.ServerKind]Backend
}

func NewRegistry(backends ...Backend) *Registry {
	r := &Registry{backends: make(map[model.ServerKind]Backend, len(backends))}
	for _, b := range backends {
		r.backends[b.Kind()] = b
	}
	return r
}

func (r *Registry) For(kind model.ServerKind) (Backend, error) {
	b, ok := r.backends[kind]
	if !ok {
		return nil, fmt.Errorf("no backend registered for server kind %q", kind)
	}
	return b, nil
}
