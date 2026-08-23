package backend

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/srinath/framewright/internal/model"
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
	downloadURL := fmt.Sprintf("http://%s:%d/Items/%s/Download", server.Host, server.Port, itemID)

	// The credential is a HEADER here, not a query parameter or
	// userinfo component the way Plex/Kodi carry theirs — confirmed in
	// docs/SERVER-NOTES.md, and the reason ExtraInputArgs exists at
	// all: ffmpeg needs `-headers "...\r\n"` placed BEFORE `-i`, which
	// package extract is responsible for actually doing; this backend
	// only has to hand the right header string back.
	return ResolvedSource{
		URL:            downloadURL,
		ExtraInputArgs: []string{"-headers", "Authorization: " + b.authHeader(server) + "\r\n"},
	}, nil
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
