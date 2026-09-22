package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/sreenathkc/cliphanger/internal/model"
)

// JellyfinBackend — UNVERIFIED against a live server, per
// docs/SERVER-NOTES.md. One call, no lookup — Jellyfin's /Download
// endpoint IS the streamable URL directly, unlike Plex (metadata lookup
// first) or Kodi (JSON-RPC lookup first).
type JellyfinBackend struct {
	client *http.Client
}

func NewJellyfinBackend() *JellyfinBackend {
	return &JellyfinBackend{client: newHTTPClient()}
}

func (b *JellyfinBackend) Kind() model.ServerKind { return model.KindJellyfin }

func (b *JellyfinBackend) authHeader(server model.Server) string {
	return "MediaBrowser Token=" + server.JellyfinToken
}

func (b *JellyfinBackend) Resolve(ctx context.Context, server model.Server, itemID string) (ResolvedSource, error) {
	// itemID escaped (2026-08-24, real finding from a security review —
	// see plex.go's own Resolve for the matching fix and full reasoning):
	// this was the other of the two backends interpolating a client-
	// supplied itemID into a URL unescaped, unlike Kodi's own
	// url.PathEscape convention for exactly this.
	downloadURL := fmt.Sprintf("http://%s:%d/Items/%s/Download", server.Host, server.Port, url.PathEscape(itemID))

	// The credential is a HEADER here, not a query parameter or
	// userinfo component the way Plex/Kodi carry theirs — confirmed in
	// docs/SERVER-NOTES.md, and the reason ExtraInputArgs exists at
	// all: ffmpeg needs `-headers "...\r\n"` placed BEFORE `-i`, which
	// package extract is responsible for actually doing; this backend
	// only has to hand the right header string back.
	return ResolvedSource{
		URL:            downloadURL,
		ExtraInputArgs: []string{"-headers", "Authorization: " + b.authHeader(server) + "\r\n"},
		Title:          b.fetchTitle(ctx, server, itemID),
	}, nil
}

// fetchTitle is a SECOND call this backend didn't previously make (this
// type's own header comment: "One call, no lookup" — no longer quite
// true, see ResolvedSource.Title's doc comment for why it's worth it
// anyway). UNVERIFIED against a live Jellyfin server, same caveat this
// whole backend already carries (docs/SERVER-NOTES.md) — /Items/{id}
// with the same MediaBrowser-token auth this backend already uses for
// everything else is the standard shape, but confirm against a real
// server before trusting it blindly. Best-effort and silent on any
// failure: Resolve must never fail just because the title lookup did —
// the file itself is what actually matters, and Job.Title already
// falls back to the redacted path when this comes back empty.
func (b *JellyfinBackend) fetchTitle(ctx context.Context, server model.Server, itemID string) string {
	itemURL := fmt.Sprintf("http://%s:%d/Items/%s", server.Host, server.Port, url.PathEscape(itemID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, itemURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", b.authHeader(server))
	req.Header.Set("Accept", "application/json")

	resp, err := b.client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}

	var parsed struct {
		Name string `json:"Name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return ""
	}
	return parsed.Name
}

func (b *JellyfinBackend) TestConnection(ctx context.Context, server model.Server) error {
	infoURL := fmt.Sprintf("http://%s:%d/System/Info", server.Host, server.Port)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, infoURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", b.authHeader(server))

	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("reaching Jellyfin server: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("Jellyfin rejected the API key — check it was copied correctly (Dashboard → API Keys)")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Jellyfin server responded %d: %s", resp.StatusCode, truncate(body, 300))
	}
	return nil
}
