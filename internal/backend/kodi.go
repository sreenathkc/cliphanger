package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/sreenathkc/cliphanger/internal/model"
)

// KodiBackend — UNVERIFIED against a live server, per
// docs/SERVER-NOTES.md. Built exactly to the documented shape; the
// three failure modes that document calls out by name (webserver
// disabled, file outside a configured source, 401-not-404 on the
// latter) are surfaced as distinct, actionable errors rather than one
// generic "request failed" — that's the whole reason those modes were
// worth writing down in the first place.
type KodiBackend struct {
	client *http.Client
}

func NewKodiBackend() *KodiBackend {
	return &KodiBackend{client: newHTTPClient()}
}

func (b *KodiBackend) Kind() model.ServerKind { return model.KindKodi }

type kodiRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	// omitempty is load-bearing: a param-less call (JSONRPC.Ping, from
	// TestConnection) passes nil here, and Kodi 19+ rejects an explicit
	// "params": null with -32600 "Invalid request." — per JSON-RPC 2.0,
	// params when present must be an array or object, never null. Verified
	// against Kodi 21.2: `{"method":"JSONRPC.Ping","params":null}` fails,
	// the same request without the key returns "pong". Symptom before this
	// fix was "unexpected reply from Kodi: \"\"" when adding a Kodi server.
	Params interface{} `json:"params,omitempty"`
}

type kodiMovieDetailsResponse struct {
	Result struct {
		MovieDetails struct {
			File string `json:"file"`
		} `json:"moviedetails"`
	} `json:"result"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (b *KodiBackend) rpc(ctx context.Context, server model.Server, method string, params interface{}, out interface{}) error {
	payload, err := json.Marshal(kodiRPCRequest{JSONRPC: "2.0", ID: 1, Method: method, Params: params})
	if err != nil {
		return err
	}
	rpcURL := fmt.Sprintf("http://%s:%d/jsonrpc", server.Host, server.Port)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rpcURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if server.KodiUsername != "" {
		req.SetBasicAuth(server.KodiUsername, server.KodiPassword)
	}

	resp, err := b.client.Do(req)
	if err != nil {
		// The most common shape of this failure IS "webserver not
		// enabled" — Kodi's HTTP control surface has to be turned on
		// explicitly (Settings → Services → Control → Allow remote
		// control via HTTP), and a connection refused here is
		// indistinguishable from the box being off. Say both.
		return fmt.Errorf("reaching Kodi's web server at %s:%d — check \"Allow remote control via HTTP\" is enabled in Kodi's Settings → Services → Control, and that the host/port are correct: %w", server.Host, server.Port, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Kodi returned %d for %s: %s", resp.StatusCode, method, truncate(body, 300))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decoding Kodi response for %s: %w — body: %s", method, err, truncate(body, 300))
	}
	return nil
}

func (b *KodiBackend) Resolve(ctx context.Context, server model.Server, itemID string) (ResolvedSource, error) {
	movieID, err := strconv.Atoi(itemID)
	if err != nil {
		return ResolvedSource{}, fmt.Errorf("Kodi item id %q is not a movieid (expected an integer): %w", itemID, err)
	}

	var details kodiMovieDetailsResponse
	err = b.rpc(ctx, server, "VideoLibrary.GetMovieDetails", map[string]interface{}{
		"movieid":    movieID,
		"properties": []string{"file"},
	}, &details)
	if err != nil {
		return ResolvedSource{}, err
	}
	if details.Error != nil {
		return ResolvedSource{}, fmt.Errorf("Kodi: %s", details.Error.Message)
	}
	if details.Result.MovieDetails.File == "" {
		return ResolvedSource{}, fmt.Errorf("Kodi movieid %d has no file path in its library entry", movieID)
	}

	// The VFS endpoint serves only paths inside a configured Kodi
	// SOURCE — a file Kodi can play is not necessarily one it will
	// serve (docs/SERVER-NOTES.md, confirmed failure mode #2). That
	// call fails with 401, not 404, when it's outside one — which reads
	// as an auth problem and sends you chasing the wrong thing unless
	// this backend says so explicitly, which is what the 401 branch
	// below does.
	encodedPath := url.PathEscape(details.Result.MovieDetails.File)
	streamURL := fmt.Sprintf("http://%s:%d/vfs/%s", server.Host, server.Port, encodedPath)

	if server.KodiUsername != "" {
		streamURL = fmt.Sprintf("http://%s:%s@%s:%d/vfs/%s",
			url.QueryEscape(server.KodiUsername), url.QueryEscape(server.KodiPassword),
			server.Host, server.Port, encodedPath)
	}

	// A quick HEAD confirms the VFS path is actually servable before
	// handing it to ffmpeg — worth the extra round trip specifically
	// because a 401-shaped failure here is easy to misdiagnose (see
	// above), and doing this check now means the job's Error can say
	// exactly that instead of ffmpeg's own opaque "server returned 401"
	// stderr.
	headReq, err := http.NewRequestWithContext(ctx, http.MethodHead, streamURL, nil)
	if err == nil {
		if resp, err := b.client.Do(headReq); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusUnauthorized {
				// Confirmed VFS-outside-source failure (see comment above) —
				// this is exactly the case LocalPath's fallback exists for
				// (2026-08-22, per direct request: "we will still need a way
				// to run the command against direct file path, in case if no
				// plex avaiable, only kodi etc"). Only engages if the user
				// has actually configured a mapping for this server; a
				// server with no mapping gets exactly the same error as
				// before, unchanged.
				if localPath, ok := LocalPath(server, details.Result.MovieDetails.File); ok {
					return ResolvedSource{URL: localPath}, nil
				}
				return ResolvedSource{}, fmt.Errorf(
					"Kodi refused to serve %q (401) — this file is outside a configured Kodi SOURCE. "+
						"Since Frodo, Kodi's webserver only serves paths inside a source; a file Kodi can PLAY is not necessarily one it will SERVE. "+
						"Add the containing folder as a source in Kodi, check credentials if one is required, or configure a local path mapping for this server in Setup so ClipHanger can read the file directly.",
					details.Result.MovieDetails.File,
				)
			}
		}
	}

	return ResolvedSource{URL: streamURL}, nil
}

func (b *KodiBackend) TestConnection(ctx context.Context, server model.Server) error {
	var pong struct {
		Result string `json:"result"`
	}
	if err := b.rpc(ctx, server, "JSONRPC.Ping", nil, &pong); err != nil {
		return err
	}
	if pong.Result != "pong" {
		return fmt.Errorf("unexpected reply from Kodi: %q", pong.Result)
	}
	return nil
}
