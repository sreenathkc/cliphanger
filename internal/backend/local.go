// LocalBackend resolves an item by treating itemID as the raw file path
// the ORIGINAL media server (Plex/Kodi/Jellyfin) reports for it, mapped
// through this server's own LocalPathFrom/LocalPathTo prefix pair into
// a path ClipHanger can read directly — no HTTP, no auth, no server API
// call at all. Added 2026-09-12, per direct request: "if cliphanger has
// an option to set/mount a path... [it] can access the file directly
// instead of plex/kodi."
//
// This promotes the same LocalPathFrom/LocalPathTo mapping Kodi's own
// backend already uses (see kodi.go) — there, a narrow, opt-in rescue
// that only ever engages after a CONFIRMED 401 on Kodi's own VFS — into
// its own deliberate, first-class server kind instead. CLAUDE.md's
// "never [read media off disk] speculatively... never wire it into a
// backend without a real, confirmed failure condition to trigger on
// first" was about a backend silently reaching for disk access behind
// the scenes; a server the user explicitly adds FOR this purpose, on
// its own line in Setup, is the opposite of speculative — it's a real
// source the client (DemoFlex) chooses to try, same as any other.
package backend

import (
	"context"
	"fmt"
	"os"

	"github.com/sreenathkc/cliphanger/internal/model"
)

type LocalBackend struct{}

func NewLocalBackend() *LocalBackend { return &LocalBackend{} }

func (b *LocalBackend) Kind() model.ServerKind { return model.KindLocal }

// Resolve treats itemID as the raw path the ORIGINAL source reports for
// this file — e.g. Kodi's own `nfs://host/share/Movie.mkv`, or Plex's
// raw filesystem path (`Part.file`) — maps it through this server's
// LocalPathFrom/LocalPathTo, then confirms the mapped path actually
// exists before calling it resolved. A clear "no such file" here, with
// the exact path that was tried, beats a cryptic ffmpeg error three
// steps downstream — and doubles as the practical way to GET the right
// prefix configured in the first place: the error names the literal raw
// path DemoFlex sent, which is exactly what LocalPathFrom needs to
// match (see setup.html's own hint for this field).
func (b *LocalBackend) Resolve(ctx context.Context, server model.Server, itemID string) (ResolvedSource, error) {
	mapped, ok := LocalPath(server, itemID)
	if !ok {
		return ResolvedSource{}, fmt.Errorf(
			"path %q doesn't start with this server's configured prefix (%q) — the prefix has to match exactly what your media server itself reports for this file's path; adjust it in Setup",
			itemID, server.LocalPathFrom,
		)
	}
	info, err := os.Stat(mapped)
	if err != nil {
		return ResolvedSource{}, fmt.Errorf("mapped path %q: %w — check the mount is actually attached inside this container and the mapping is correct", mapped, err)
	}
	if info.IsDir() {
		return ResolvedSource{}, fmt.Errorf("mapped path %q is a directory, not a file", mapped)
	}
	return ResolvedSource{URL: mapped}, nil
}

// TestConnection has no server to ping — no host, no credentials,
// nothing network-facing about this kind at all. What it verifies
// instead, same spirit as the *arr suite's own "root folder" check:
// the mount ClipHanger is supposed to read from actually exists, is a
// directory, and is genuinely readable from INSIDE THIS CONTAINER —
// the three ways "I configured a path" and "ClipHanger can actually use
// it" diverge in practice (the mount never got attached to the
// container, a typo in the path, or a permissions mismatch between the
// host and the container's own user). Doesn't (and can't) confirm the
// LocalPathFrom half is right — that half can only ever be checked
// against a real item's real reported path, which is what a failed
// Resolve's own error message is for.
func (b *LocalBackend) TestConnection(ctx context.Context, server model.Server) error {
	// LocalPathFrom is deliberately NOT required here (2026-09-13, real
	// report: requiring it up front made adding a server and THEN
	// learning the right value via Server.LastAttemptedPath impossible —
	// you'd never get past this check to generate the first attempt
	// that teaches you the value). Only LocalPathTo — the actual mount —
	// is something this check can verify at all; see this function's
	// own doc comment for why the other half can't be.
	if server.LocalPathTo == "" {
		return fmt.Errorf("ClipHanger's own path is required for a local mount")
	}
	info, err := os.Stat(server.LocalPathTo)
	if err != nil {
		return fmt.Errorf("can't see %q from inside this container: %w — is the mount actually attached?", server.LocalPathTo, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%q exists but isn't a directory", server.LocalPathTo)
	}
	entries, err := os.ReadDir(server.LocalPathTo)
	if err != nil {
		return fmt.Errorf("%q exists but isn't readable: %w", server.LocalPathTo, err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("%q is empty — right mount, but nothing in it (double-check the path)", server.LocalPathTo)
	}
	return nil
}
