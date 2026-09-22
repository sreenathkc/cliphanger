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

// PlexBackend — CONFIRMED against a live server, per docs/SERVER-NOTES.md.
type PlexBackend struct {
	client *http.Client
}

func NewPlexBackend() *PlexBackend {
	return &PlexBackend{client: newHTTPClient()}
}

func (b *PlexBackend) Kind() model.ServerKind { return model.KindPlex }

// plexMetadataResponse is deliberately narrow — only the fields this
// backend actually reads. ratingKey itself is NOT decoded here (this
// backend is handed the ratingKey as itemID already); if a future
// change needs it, remember docs/SERVER-NOTES.md's confirmed trap:
// "ratingKey comes back as a JSON string in some responses and an Int
// in others. Accept either."
type plexMetadataResponse struct {
	MediaContainer struct {
		Metadata []struct {
			// title (2026-09-22): plain top-level field on every Plex
			// metadata entry, same response this backend already fetches
			// to resolve the file — see ResolvedSource.Title's own doc
			// comment.
			Title string `json:"title"`
			Media []struct {
				Part []struct {
					Key string `json:"key"`
				} `json:"Part"`
			} `json:"Media"`
		} `json:"Metadata"`
	} `json:"MediaContainer"`
}

func (b *PlexBackend) Resolve(ctx context.Context, server model.Server, itemID string) (ResolvedSource, error) {
	// itemID/PlexToken escaped (2026-08-24, real finding from a security
	// review) — this was building the URL by raw string interpolation,
	// the one place in the three backends that didn't already follow
	// Kodi's own established url.PathEscape/QueryEscape convention for
	// exactly this. Low real-world severity given a caller already needs
	// a valid ClipHanger API key to reach this at all (same trust level
	// this whole service already assumes — see docs/DECISIONS.md), but a
	// genuinely wrong pattern regardless: an itemID containing `?`, `#`,
	// or similar could otherwise mangle the request in ways that have
	// nothing to do with malice — a legitimately weird itemID breaking a
	// request silently and confusingly is reason enough on its own.
	metaURL := fmt.Sprintf("http://%s:%d/library/metadata/%s?X-Plex-Token=%s",
		server.Host, server.Port, url.PathEscape(itemID), url.QueryEscape(server.PlexToken))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metaURL, nil)
	if err != nil {
		return ResolvedSource{}, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := b.client.Do(req)
	if err != nil {
		return ResolvedSource{}, fmt.Errorf("reaching Plex server: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return ResolvedSource{}, fmt.Errorf("Plex returned %d for item %s: %s", resp.StatusCode, itemID, truncate(body, 300))
	}

	var parsed plexMetadataResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		// Surface the raw body, not a bare decode error — CLAUDE.md:
		// "when a response doesn't decode, surface the raw body in the
		// error rather than a bare decode failure. That practice caught
		// several wrong assumptions during DemoFlex's development."
		return ResolvedSource{}, fmt.Errorf("decoding Plex metadata for item %s: %w — body: %s", itemID, err, truncate(body, 300))
	}
	if len(parsed.MediaContainer.Metadata) == 0 ||
		len(parsed.MediaContainer.Metadata[0].Media) == 0 ||
		len(parsed.MediaContainer.Metadata[0].Media[0].Part) == 0 {
		return ResolvedSource{}, fmt.Errorf("Plex item %s has no Media/Part in its metadata — body: %s", itemID, truncate(body, 300))
	}

	// partKey itself stays unescaped — it's Plex's OWN response (already
	// a correctly-formed URL path segment, e.g. "/library/parts/12345/
	// file.mkv"), not client-supplied, so it's the same trusted-server-
	// data case Kodi's own resolved file path is, not the itemID case
	// above. Only the token (still ours to get right) gets escaped.
	partKey := parsed.MediaContainer.Metadata[0].Media[0].Part[0].Key
	streamURL := fmt.Sprintf("http://%s:%d%s?X-Plex-Token=%s", server.Host, server.Port, partKey, url.QueryEscape(server.PlexToken))
	return ResolvedSource{URL: streamURL, Title: parsed.MediaContainer.Metadata[0].Title}, nil
}

func (b *PlexBackend) TestConnection(ctx context.Context, server model.Server) error {
	identityURL := fmt.Sprintf("http://%s:%d/identity", server.Host, server.Port)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, identityURL, nil)
	if err != nil {
		return err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("reaching Plex server: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Plex server responded %d", resp.StatusCode)
	}
	return nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
