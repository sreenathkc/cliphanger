package plexauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// realisticResourcesXML mirrors an ACTUAL /api/v2/resources response
// shape — including the many extra attributes a real response carries
// that this package doesn't care about (productVersion, platform,
// owned, home, synced, relay, presence, etc.), to prove unknown
// attributes/elements don't break parsing. Includes a non-server
// resource (a phone) to prove the provides filter actually filters,
// and a server with TWO connections (remote first, local second) to
// prove the "prefer local" logic picks the right one regardless of
// order.
const realisticResourcesXML = `<?xml version="1.0" encoding="UTF-8"?>
<resources size="2" friendlyName="someone@example.com">
  <resource name="Living Room Shield" product="Plex Media Server" productVersion="1.41.0" platform="Linux" platformVersion="" device="PC" clientIdentifier="abc123serveridentifier" createdAt="1700000000" lastSeenAt="1755000000" provides="server" ownerId="" sourceTitle="" publicAddress="203.0.113.5" accessToken="ignored-in-this-response" owned="1" home="1" synced="0" relay="1" dnsRebindingProtection="1" natLoopbackSupported="1" publicAddressMatches="1" presence="1" httpsRequired="0">
    <connection protocol="http" address="203.0.113.5" port="32400" uri="http://203.0.113.5:32400" local="0" relay="0" IPv6="0"/>
    <connection protocol="http" address="192.168.1.50" port="32400" uri="http://192.168.1.50:32400" local="1" relay="0" IPv6="0"/>
  </resource>
  <resource name="Someone's iPhone" product="Plex" productVersion="10.5" platform="iOS" platformVersion="17.0" device="iPhone" clientIdentifier="def456phoneidentifier" createdAt="1700000001" lastSeenAt="1755000001" provides="player,pubsub-player,provider-playback" ownerId="" sourceTitle="" publicAddress="203.0.113.6" accessToken="" owned="1" home="1" synced="0" relay="0" dnsRebindingProtection="1" natLoopbackSupported="1" publicAddressMatches="1" presence="1" httpsRequired="0">
    <connection protocol="http" address="203.0.113.6" port="32500" uri="http://203.0.113.6:32500" local="0" relay="0" IPv6="0"/>
  </resource>
</resources>`

func TestFetchServers_ParsesRealisticResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/resources" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("X-Plex-Token"); got != "test-token" {
			t.Fatalf("expected X-Plex-Token header, got %q", got)
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(realisticResourcesXML))
	}))
	defer srv.Close()

	// FetchServers hardcodes apiBase — point it at the test server for
	// the duration of this test, same pattern as any package-level base
	// URL override in a test.
	origBase := apiBase
	apiBase = srv.URL
	defer func() { apiBase = origBase }()

	servers, err := FetchServers(context.Background(), "test-client-id", "test-token")
	if err != nil {
		t.Fatalf("FetchServers returned an error: %v", err)
	}
	if len(servers) != 1 {
		t.Fatalf("expected exactly 1 server (the player device must be filtered out), got %d: %+v", len(servers), servers)
	}
	got := servers[0]
	if got.Name != "Living Room Shield" {
		t.Errorf("Name = %q, want %q", got.Name, "Living Room Shield")
	}
	// The local connection was listed SECOND in the XML — this proves
	// the "prefer local" logic actually looks at all connections rather
	// than just taking the first one.
	if got.Host != "192.168.1.50" {
		t.Errorf("Host = %q, want the LOCAL connection's address %q (not the remote one listed first)", got.Host, "192.168.1.50")
	}
	if got.Port != 32400 {
		t.Errorf("Port = %d, want 32400", got.Port)
	}
}

func TestFetchServers_EmptyAccountReturnsNoServersNotError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><resources size="0"></resources>`))
	}))
	defer srv.Close()
	origBase := apiBase
	apiBase = srv.URL
	defer func() { apiBase = origBase }()

	servers, err := FetchServers(context.Background(), "test-client-id", "test-token")
	if err != nil {
		t.Fatalf("expected no error for a genuinely empty account, got: %v", err)
	}
	if len(servers) != 0 {
		t.Fatalf("expected 0 servers, got %d", len(servers))
	}
}
