package store

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// generateAPIKey produces a long, high-entropy secret — this is what
// stands between the LAN and a service that owns real media-server
// credentials (CLAUDE.md: "the UI manages credentials, so it must not
// sit open on the LAN behind nothing"). 32 random bytes, hex-encoded.
func generateAPIKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating API key: %w", err)
	}
	return "ch_" + hex.EncodeToString(buf), nil
}

// generateServerID produces the srv_XXXX-shaped id used throughout
// docs/API.md's examples — short, since it appears in every job's
// source.serverId and gets typed/copied by hand more than the API key
// does.
func generateServerID() (string, error) {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating server id: %w", err)
	}
	return "srv_" + hex.EncodeToString(buf), nil
}

// generateClientIdentifier produces this install's plex.tv client
// identity — doesn't need to be a real RFC 4122 UUID, plex.tv accepts
// any stable, sufficiently unique string for X-Plex-Client-Identifier.
func generateClientIdentifier() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating client identifier: %w", err)
	}
	return "cliphanger-" + hex.EncodeToString(buf), nil
}
