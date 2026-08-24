package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

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
			Media []struct {
				Part []struct {
					Key string `json:"key"`
				} `json:"Part"`
			} `json:"Media"`
		} `json:"Metadata"`
	} `json:"MediaContainer"`
}

func (b *PlexBackend) Resolve(ctx context.Context, server model.Server, itemID string) (ResolvedSource, error) {
	metaURL := fmt.Sprintf("http://%s:%d/library/metadata/%s?X-Plex-Token=%s",
		server.Host, server.Port, itemID, server.PlexToken)

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

	partKey := parsed.MediaContainer.Metadata[0].Media[0].Part[0].Key
	streamURL := fmt.Sprintf("http://%s:%d%s?X-Plex-Token=%s", server.Host, server.Port, partKey, server.PlexToken)
	return ResolvedSource{URL: streamURL}, nil
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
