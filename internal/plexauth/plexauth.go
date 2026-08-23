// Package plexauth is the "Sign in with Plex" PIN/link flow (2026-08-22,
// per direct request — a friendlier alternative to hunting down a token
// by hand via View XML). Confirmed live against the real plex.tv API
// while building this (not assumed from docs): POST /api/v2/pins with
// strong=true returns a long code meant for a direct one-click link
// (app.plex.tv/auth#?...) rather than the short 4-character code Plex's
// TV-remote-input flow uses — appropriate here since the web UI runs in
// a real browser, not a limited-input device.
//
// This is Framewright's first-ever call to plex.tv itself — everything
// else this service does only ever talks to the media server on your
// own LAN (docs/DECISIONS.md). Deliberately isolated in its own package
// so that boundary stays visible and easy to audit, not folded into
// backend.PlexBackend (which resolves items on an ALREADY-configured
// server and has no reason to know plex.tv exists at all).
package plexauth

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// apiBase is a var, not a const, specifically so plexauth_test.go can
// point it at an httptest.Server — this package's whole point is being
// the one thing in Framewright that talks to plex.tv itself, so testing
// it for real (against a mock plex.tv, not the live one) matters more
// here than almost anywhere else in the codebase.
var (
	apiBase  = "https://plex.tv"
	authBase = "https://app.plex.tv"
)

var httpClient = &http.Client{Timeout: 10 * time.Second}

// accessTokenAttrPattern strips accessToken="..." (any casing) before
// anything from a real /resources body ever reaches a log line — a real
// response carries each resource's OWN access token as an attribute,
// not just ours. Same "never let a credential reach a log line" rule
// backend.redact() enforces for ffmpeg/HTTP errors, applied here to
// this endpoint's specific shape.
var accessTokenAttrPattern = regexp.MustCompile(`(?i)accessToken="[^"]*"`)

// Session is a pin awaiting approval.
type Session struct {
	ID      int
	AuthURL string
}

type pinResponse struct {
	ID               int    `json:"id"`
	Code             string `json:"code"`
	ClientIdentifier string `json:"clientIdentifier"`
	AuthToken        string `json:"authToken"`
}

// CreatePin requests a new pin tied to clientIdentifier (Framewright's
// own persisted identity — see store.Store.ClientIdentifier) and
// returns a URL the user opens to approve it. product is shown to the
// user on Plex's own consent screen ("Framewright wants to link this
// device").
func CreatePin(ctx context.Context, clientIdentifier, product string) (Session, error) {
	// strong=true as FORM BODY data, not a query param — confirmed live
	// against the real API while building this (a query-param version
	// was not tested and is not assumed to work).
	form := url.Values{"strong": {"true"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+"/api/v2/pins", strings.NewReader(form.Encode()))
	if err != nil {
		return Session{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	setPlexHeaders(req, clientIdentifier, product)

	resp, err := httpClient.Do(req)
	if err != nil {
		return Session{}, fmt.Errorf("reaching plex.tv: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return Session{}, fmt.Errorf("plex.tv returned %d creating a pin: %s", resp.StatusCode, truncate(body, 300))
	}

	var parsed pinResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Session{}, fmt.Errorf("decoding plex.tv pin response: %w — body: %s", err, truncate(body, 300))
	}

	authURL := fmt.Sprintf(
		"%s/auth#?clientID=%s&code=%s&context[device][product]=%s",
		authBase,
		url.QueryEscape(clientIdentifier),
		url.QueryEscape(parsed.Code),
		url.QueryEscape(product),
	)
	return Session{ID: parsed.ID, AuthURL: authURL}, nil
}

// Poll checks whether the user has approved pinID yet. Returns an empty
// string, no error, while still pending — that's the normal "not yet"
// case, not a failure. clientIdentifier MUST be the exact same value
// used in CreatePin; plex.tv ties the two together and silently returns
// no token otherwise.
func Poll(ctx context.Context, clientIdentifier string, pinID int) (authToken string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/api/v2/pins/%d", apiBase, pinID), nil)
	if err != nil {
		return "", err
	}
	setPlexHeaders(req, clientIdentifier, "")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("reaching plex.tv: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("this sign-in link expired — start again")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("plex.tv returned %d checking the pin: %s", resp.StatusCode, truncate(body, 300))
	}

	var parsed pinResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("decoding plex.tv pin response: %w — body: %s", err, truncate(body, 300))
	}
	return parsed.AuthToken, nil
}

// DiscoveredServer is one Plex Media Server tied to the signed-in
// account, ready to fill in Setup's host/port fields directly.
type DiscoveredServer struct {
	Name string `json:"name"`
	Host string `json:"host"`
	Port int    `json:"port"`
}

// resourceParsed/connectionParsed are what parseResources extracts.
// Deliberately NOT decoded via encoding/xml struct tags — that was the
// first version of this code, and it was a real, live-confirmed bug
// (2026-08-23): every resource in a real account's response parsed
// with 0 connections, servers included, because struct-tag decoding
// matches element names EXACTLY, and this endpoint's real casing
// doesn't match what "confirmed 2026-08-22" had assumed from a
// smaller/different sample. DemoFlex's own reference parser
// (PlexResourcesXMLParser in PlexAuthService.swift) already matches
// element AND attribute names case-INsensitively for exactly this
// reason — its own comment says so explicitly. This ports that same
// defensiveness properly instead of re-hitting the bug it was written
// to avoid.
type resourceParsed struct {
	Name        string
	Provides    string
	Connections []connectionParsed
}

type connectionParsed struct {
	Address string
	Port    int
	Local   string // "1" or "0"
}

// parseResources walks the XML token by token, matching element and
// attribute names with strings.EqualFold — robust to whatever casing
// this endpoint actually uses, rather than assuming one.
func parseResources(body []byte) ([]resourceParsed, error) {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	var resources []resourceParsed
	var current *resourceParsed
	for {
		tok, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch {
			case strings.EqualFold(t.Name.Local, "resource"):
				r := resourceParsed{}
				for _, a := range t.Attr {
					switch {
					case strings.EqualFold(a.Name.Local, "name"):
						r.Name = a.Value
					case strings.EqualFold(a.Name.Local, "provides"):
						r.Provides = a.Value
					}
				}
				current = &r
			case strings.EqualFold(t.Name.Local, "connection"):
				if current == nil {
					continue
				}
				c := connectionParsed{}
				for _, a := range t.Attr {
					switch {
					case strings.EqualFold(a.Name.Local, "address"):
						c.Address = a.Value
					case strings.EqualFold(a.Name.Local, "port"):
						if p, err := strconv.Atoi(a.Value); err == nil {
							c.Port = p
						}
					case strings.EqualFold(a.Name.Local, "local"):
						c.Local = a.Value
					}
				}
				current.Connections = append(current.Connections, c)
			}
		case xml.EndElement:
			if strings.EqualFold(t.Name.Local, "resource") && current != nil {
				resources = append(resources, *current)
				current = nil
			}
		}
	}
	return resources, nil
}

// FetchServers lists the Plex Media Servers reachable on the signed-in
// account, preferring each one's local (LAN) connection over a
// relay/remote one — this only ever configures servers Framewright will
// reach directly over the LAN. clientIdentifier must be the same value
// used for the pin (plex.tv ties resources to whichever identity is
// asking).
func FetchServers(ctx context.Context, clientIdentifier, token string) ([]DiscoveredServer, error) {
	// X-Plex-Token as a QUERY PARAMETER, not just a header — fixed
	// 2026-08-23 after a real report ("no server found automatically",
	// while DemoFlex's own sign-in finds the same account's server
	// fine). Comparing against DemoFlex's own PlexAuthService.swift line
	// by line turned up this exact difference: it passes X-Plex-Token
	// as a query item on /resources specifically, not (only) a header.
	// Sent as both here, belt-and-braces, since the header alone is
	// what every other call in this package uses successfully.
	reqURL := fmt.Sprintf("%s/api/v2/resources?includeHttps=1&includeRelay=1&X-Plex-Token=%s", apiBase, url.QueryEscape(token))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Plex-Client-Identifier", clientIdentifier)
	req.Header.Set("X-Plex-Token", token)
	req.Header.Set("X-Plex-Product", "Framewright")
	req.Header.Set("X-Plex-Device-Name", "Framewright")
	// Deliberately no Accept: application/json — /resources doesn't
	// reliably honor it (same finding DemoFlex's client independently
	// made hitting this exact endpoint); XML is what actually comes
	// back regardless of what's asked for.

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reaching plex.tv: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("plex.tv returned %d listing servers: %s", resp.StatusCode, truncate(body, 300))
	}

	resources, err := parseResources(body)
	if err != nil {
		return nil, fmt.Errorf("decoding plex.tv /resources: %w — body: %s", err, truncate(body, 500))
	}

	var servers []DiscoveredServer
	for _, r := range resources {
		if !strings.Contains(r.Provides, "server") || len(r.Connections) == 0 {
			continue
		}
		chosen := r.Connections[0]
		for _, c := range r.Connections {
			if c.Local == "1" {
				chosen = c
				break
			}
		}
		servers = append(servers, DiscoveredServer{Name: r.Name, Host: chosen.Address, Port: chosen.Port})
	}

	// Diagnostic for the "0 servers, but the request genuinely
	// succeeded" case (2026-08-23, added after a live report this
	// happened for a real account — which is what surfaced the casing
	// bug parseResources' own doc comment describes) — logs a SAFE
	// summary, never the raw body: a real /resources response includes
	// each resource's own accessToken attribute, and CLAUDE.md's rule is
	// that no credential (ours or anyone else's) ever reaches a log
	// line. name/provides/connection-count carry everything needed to
	// tell "genuinely no resources," "resources exist but none are
	// servers," and "a server exists but reported no connections" apart,
	// without the token risk.
	if len(servers) == 0 {
		summary := make([]string, 0, len(resources))
		for _, r := range resources {
			summary = append(summary, fmt.Sprintf("{name=%q provides=%q connections=%d}", r.Name, r.Provides, len(r.Connections)))
		}
		slog.Warn("plexauth: FetchServers found 0 matching servers", "totalResources", len(resources), "resources", summary,
			// Scrubbed raw body, on top of the summary above — the
			// case-insensitive parser fix shipped alongside this is a
			// real, independent improvement either way, but this is
			// what gives DEFINITIVE ground truth if 0 servers still
			// happens after it: the actual tag structure, not a guess
			// at it. accessTokenAttrPattern strips every resource's own
			// accessToken value first — same "never let a credential
			// reach a log line" rule as backend.redact(), applied to
			// this shape specifically.
			"rawBodyScrubbed", truncate([]byte(accessTokenAttrPattern.ReplaceAllString(string(body), `accessToken="REDACTED"`)), 2000))
	}

	return servers, nil
}

func setPlexHeaders(req *http.Request, clientIdentifier, product string) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Client-Identifier", clientIdentifier)
	if product != "" {
		req.Header.Set("X-Plex-Product", product)
	}
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
