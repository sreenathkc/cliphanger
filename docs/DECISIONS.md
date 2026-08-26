# Decisions

Settled during design (August 2026). Each records the reasoning, because
the reasoning is what tells you whether a future change is safe. Don't
reverse one without understanding why it was made.

---

## Distribution

**Multi-arch container is the primary install path; a static binary
ships alongside.**

The Plex/Jellyfin/Kodi audience is the same population that runs the
*arr stack (Sonarr, Radarr, Bazarr, Tautulli, Overseerr) — where a
compose file *is* the documentation people read first. A Windows
executable was the original plan and was rejected: it excludes most
self-hosters, requires a second always-on machine, and needs code
signing before anyone can run it without warnings.

**arm64 is required, not optional.** Synology ARM models, Raspberry Pis
and Apple Silicon dev machines are all arm64. An amd64-only image
excludes exactly the people most likely to self-host this.

**Written in a compiled single-binary language (Go or Rust).** This is a
distribution decision, not a taste one: it yields both a small container
and a static binary from one source. An interpreted runtime would make
Docker effectively mandatory.

**ffmpeg is an external dependency**, installed from the distro in the
image, never bundled into the binary or image build. Redistributing a
compiled ffmpeg carries GPL/LGPL obligations that vary with exactly how
it was built (which codecs/libraries were compiled in) — treating it as
something the runtime environment provides, the same way a Python
package doesn't vendor the Python interpreter, avoids taking on those
obligations at all.

---

## The service is general-purpose

DemoFlex is its first client, not its owner. The API previously used
`bookmarkId` (formatted `imdb|tmdb|timestamp`) — an app concept — which
became a client-chosen opaque `captureId`. Anyone should be able to
write a second client without reading DemoFlex's source.

---

## Media is streamed over HTTP, never read from disk

ffmpeg opens an HTTP URL as happily as a file. Streaming from the media
server means:

- No NAS mount on the ClipHanger host.
- No path translation between what the server sees and what the host
  sees (drive letters, permissions, differing mount points).
- It works no matter which machine it runs on.

The trade-off is one extra network hop, which matters most for Kodi (see
`SERVER-NOTES.md` — bytes travel NAS → Kodi box → ClipHanger).

**Narrow exception added 2026-08-22** — a server can optionally
configure `LocalPathFrom`/`LocalPathTo` (a prefix mapping, the same
"remote path mapping" pattern *arr-stack tools use). Today it's wired
into exactly one place: Kodi's `Resolve()`, only after a HEAD check
confirms the VFS endpoint actually returned `401` for a specific item
(the outside-a-configured-source failure mode). This resolves what used
to be an open question here ("Kodi VFS reliability — unknown how often
that bites a real library") with a real mitigation rather than an
answer to the unknown — Kodi's own reliability is unchanged, but a
user who hits it now has a way through instead of a dead end. Deliberately
NOT extended to Plex or Jellyfin, which don't have a confirmed failure
mode to trigger on, and NOT a "read files directly" general mode — see
`CLAUDE.md`'s hard rule for the boundary.

---

## No privileged backend

A Kodi-only or Jellyfin-only setup must work fully. Jobs therefore name
a **source** (`serverId` + that server's own `itemId`) rather than
anything Plex-shaped. Only the URL-resolution step varies by backend;
everything downstream sees a URL.

---

## The web UI owns credentials

Reverses an earlier decision that clients would send them.

Two reasons. First, every comparable self-hosted tool answers on its
port — one that returns a bare JSON 404 to a browser reads as broken.
Second, and more important: **without a UI, the service can't be
configured without a client.** That's a poor first run for an
open-source project and forces every future client to implement a
credential-entry flow.

So credentials are entered on the box that stores them and never travel.
Clients hold an API key and read `GET /servers` (credential-free) to
discover what's configured.

**Server-rendered HTML + htmx, not a SPA.** Templates embedded in the
binary preserve the single-static-binary promise and keep Node out of
CI. Three pages don't justify a second toolchain.

---

## Web UI is open by default, not Basic-Auth-walled (revised 2026-08-23)

The original design put the whole web UI behind HTTP Basic Auth, using
the API key as the password (docs/API.md used to say "must not sit
open on the LAN behind nothing — put it behind the same API key at
minimum"). Reversed after direct, repeated pushback — this made first
login genuinely circular in practice: the ONLY place the auto-generated
key was ever shown was the Setup page itself, which was the thing
behind the wall. Moving the key to `docker logs` didn't actually fix
the complaint; it was the wall itself, not where the key lived.

Compared against how actually-comparable self-hosted tools behave —
Sonarr, Radarr, Prowlarr ship with **no login at all** by default; you
opt into a username/password later from Settings if you want one.
Overseerr goes further and uses "Sign in with Plex" AS its own admin
login, no separate app password at all. None of them wall off first
contact with an auto-generated secret.

The web UI is now open on the LAN, same posture as those tools — this
assumes a trusted home network, not direct public exposure (see
"Transport security" in Open Questions below, still unresolved). The
API key still exists and still matters: it's what CLIENTS (like
DemoFlex) send in `X-Api-Key` to use the actual extraction API, and
that surface is unchanged. It just no longer doubles as a human login
password too — those were two different jobs wearing one credential,
and conflating them is what caused the circularity in the first place.

An optional real username/password for the web UI (matching Sonarr's
"add one later from Settings if you want it" model) is a reasonable
future addition, not built here — this change is scoped to removing
the mandatory wall, not building session-based auth.

**Built 2026-08-25** — the "future addition" above, per direct request
("radarr etc have an option to enable local login, once enabled local
user will need to login even while accessing locally"). Exactly the
optional, opt-in shape already sketched out: default stays open; a new
Security page (API key moved there too, alongside Setup getting
crowded — see "Setup page split into Setup + Security" below) lets an
admin set a username/password and flip local login on, and ONLY THEN
does every web UI page (Setup/Jobs/Health/Security) start requiring a
signed session cookie, checked fresh on every request via `requireLogin`
in `internal/web/web.go`. The bootstrap order avoids the exact
circularity that sank the original Basic-Auth design: credentials are
configured while the UI is still open, before the wall goes up, not
the other way around.

Deliberately scoped to `internal/web`'s HTML pages only — `internal/api`
(the JSON API DemoFlex and other clients use) keeps authenticating with
the API key alone, completely unaffected by this toggle either way. A
browser session and an API key are different credentials guarding
different surfaces; requiring both for API calls that already carry a
valid key would add friction with no real security gain, since the key
itself is already the access control there.

Session cookie is a stateless, HMAC-signed token (`internal/web/session.go`)
keyed by a new persisted `SessionSecret` (same generate-once pattern as
`APIKey`/`ClientIdentifier`) — no session table needed, consistent with
the whole store staying one plain JSON file. 30-day expiry: a home-LAN
settings UI, not a bank; forcing daily re-logins would be pure friction
for no real security gained on a network already trusted enough to
self-host media extraction on. Password hashing is bcrypt
(`golang.org/x/crypto/bcrypt`, pinned to v0.36.0 — the newest release
whose own `go.mod` still only requires go1.23.0, matching the
`golang:1.23-bookworm` Dockerfile base rather than dragging that image
version along as a side effect of one dependency).

---

## Setup page split into Setup + Security (2026-08-25)

Adding Local Login (above) as an eighth section would have made the
Setup page's card list start feeling like a wall of settings rather
than a page — flagged directly ("you can change the menu items/
organise if the setup page is getting too busy"), taken as license to
reorganize rather than just append. The API key section moved out
alongside it: both it and Local Login are fundamentally the same
question — "who's allowed in, and how" — just for two different
surfaces (the JSON API vs. this web UI), so grouping them under one new
Security nav tab reads as one coherent page rather than an arbitrary
split. Setup keeps everything that's actually about extraction/server
config: Retention, Clip duration, Playback speed, the ffmpeg preview,
and Media servers.

---

## Submit-and-poll, not request-response

Extraction takes seconds to minutes. Clients submit a batch, get an
immediate acknowledgement, and poll later; media is fetched lazily per
capture rather than pushed. ClipHanger is the store of record.

`captureId` doubles as the idempotency key, so a client can re-send its
entire list on every startup without tracking what it already asked for.

**Cross-device reuse without ClipHanger knowing what a "movie" is**
(design decided 2026-08-22, **implemented 2026-08-24** in DemoFlex's own
`ClipHangerMediaClient.swift` — direct report that prompted actually
building it: deleting and re-adding the same bookmark, or even just
tapping Generate twice, always redid the full extraction, never reused
what ClipHanger already had): a second phone can discover media
ClipHanger already generated for the same scene, with zero coordination
and zero ClipHanger API changes, because the CLIENT derives `captureId`
deterministically instead of randomly —
`sha256(imdbId + tmdbId + timestamp + runtimeFingerprint + spanSeconds)`
(DemoFlex's actual formula omits `fps`, since it never sends that field
at all). Two devices with the same bookmark (same movie, same
edition/encode, same scene) independently compute the identical id and
just `GET /jobs/{that-id}`; if it's already Done, that's the "sync."
This was chosen over teaching ClipHanger to understand movie/scene
identity (a real alternative that was considered and rejected — it
would mean adding explicit title/IMDB-id/runtime fields plus a
lookup-by-those endpoint) specifically because it needed no change here
at all — the idempotency behavior above already did the work, once a
client committed to deriving the id this way, which is exactly what
happened.

**Known gap, not addressed**: `speedMultiplier` (see "Playback speed"
below) is Setup-page-only, never sent by DemoFlex, so it can't go into
this hash. A server-side speed-default change can leave a
deterministic-id lookup silently returning a clip made under the old
setting. `force: true` exists as an escape hatch; nothing calls it for
this reason yet.

---

## Output is fixed, not negotiated

Clients don't choose formats. A still is JPEG (≤1280px wide); a clip is
an MP4/H.264 (`spanSeconds` at `fps`, 20s at 10fps by default — GIF
until 2026-08-23, replaced once a real clip needed to run longer than a
few seconds without exploding in file size, see the MP4 entry below).
Fewer knobs, fewer support questions, and the defaults suit the use
case. `spanSeconds`, `fps` and `speedMultiplier` (2026-08-24 — see the
"Playback speed" entry below) are adjustable per job; nothing else is.
`speedMultiplier` is the one exception to "clients choose their own
defaults, the server just falls back": it's Setup-page-only in
practice, since DemoFlex deliberately doesn't set it per job (unlike
`spanSeconds`, which it always sends explicitly) — the server's own
configured default is what actually governs every real submission.

Clip encoding switched from CRF (constant quality) to a fixed 200 kbps
bitrate on 2026-08-24, per direct request — "a 1080 movie and a 4k UHD
HDR movie... ideally both format should generate same file output."
Source resolution/HDR were never actually why size varied (both already
get normalized away by the fixed 480px width and 8-bit `yuv420p`
output) — CRF just spends however many bits a scene NEEDS, so size
tracked content complexity instead: two SDR sources at identical
settings on the same box produced 186829 and 1194204 bytes, a 6x
spread, from content alone. Constant bitrate makes every clip converge
on roughly `spanSeconds × 25 KB` regardless of what the source looked
like, at the cost of a busy scene now compressing softer rather than
growing the file.

---

## Playback speed: condense more of the scene, not just more seconds (2026-08-24)

A demo-worthy moment can run several minutes; a fixed-length clip at
1x only ever shows a short slice of it, missing most of the scene's
actual context. `speedMultiplier` reads MORE source than the output
spans and time-compresses it via ffmpeg's `setpts` filter — at the
default 2x, a 20s clip covers 40s of the source. `setpts` runs before
`fps` in the filter chain deliberately: it rescales input frame
timestamps first, so the fps filter resamples against the
already-compressed timeline rather than the original one — reversed,
the frame rate would come out wrong. Bounded 1–8x on the Setup page;
past 8x, reading 2+ minutes of source for a 20s clip stopped buying
enough legibility in the compressed result to be worth the extra
extraction time.

Setup-page-only, not a DemoFlex-side per-request control, by direct
decision — unlike `spanSeconds`. The Setup page also shows the actual
ffmpeg command a job will run, given the current settings — read-only
for now, a possible editable "advanced mode" later. The web UI's own
copy of the filter-chain string is a hand-kept mirror of
`extract.Clip`'s real one, not shared code — if that function's flags
ever change, the preview needs updating alongside it.

---

## Clip format: MP4/H.264, not GIF (2026-08-23)

`Clip()` now encodes `libx264`/MP4 (`-preset veryfast -crf 23 -pix_fmt
yuv420p -movflags +faststart`) instead of a GIF. GIF has no inter-frame
compression, so its size scales almost linearly with `spanSeconds × fps`
— fine for the original 3s default, but a real request to preview more
of the scene (not just a few seconds) made GIF's size genuinely
prohibitive at any span worth calling a "clip" rather than a flipbook.
`probeGIF` was renamed `probeClip` (same ffprobe technique, codec-
agnostic); `handleMedia`'s Content-Type is `video/mp4`. The still stays
JPEG — this only ever affected the animated output.

---

## GIF palette: plain, not content-optimized (2026-08-22, historical)

Superseded by the MP4 switch directly above — kept here for the
`palettegen` investigation record, not because GIF encoding still
happens. `Clip()` used ffmpeg's built-in default GIF palette, not the
`palettegen`/`paletteuse` two-pass technique that's the usual best
practice for GIF quality. Reliability was chosen over quality here,
deliberately, after finding this the hard way: `palettegen`'s cost
scales badly (worse than linearly) with the number of distinct colors
in the input, and a real grainy Blu-ray-remux source pushed a single
3-second clip's palette analysis alone past 11 minutes — confirmed
directly against the file with no network involved, so this has nothing
to do with Plex, seeking, or I/O; it's `palettegen` itself on
high-noise/high-grain content. See `docs/SERVER-NOTES.md`'s Clip section
for the full elimination process.

The accepted trade-off: flatter colour and visible banding in dark or
grainy scenes, in exchange for every job finishing in well under a
second of encode time regardless of source content. Checked visually
against real output before accepting — acceptable for a demo-scene
preview GIF, which doesn't need archival-grade colour fidelity.
`JOB_TIMEOUT_SECONDS` (added the same day, for a different reason) is
no longer load-bearing for a healthy source now that this is fixed, but
stays as a genuine backstop for other slow-source cases.

---

## Retention (2026-08-22, revised same day)

First resolved as a bounded cache with a 72h default sweep. Revised
same day per direct request ("we can keep the retention, by default
forever, user can change in settings") — **the default is now forever**
(`RetentionHours = 0`), and the setting is a live, persisted value
changeable from the Setup page at any time, not a fixed value read once
from an env var at startup. `RETENTION_HOURS` still exists but only
seeds the initial value on a fresh install (`Store.SeedRetentionHoursIfUnset`,
same one-time-seed pattern as `API_KEY`) — after that, the Setup page is
authoritative and survives restarts.

The sweep mechanism itself is unchanged: a background pass
(`Queue.reapOnce`, every 15 minutes) deletes a Done or Failed job's
media and record once it's older than the current retention window,
read live from the store on every sweep. Queued/Running jobs are never
touched regardless of age. A user who wants the original bounded-cache
behavior can still set a number of hours; forever is now just the
starting point, not a fixed architectural stance — the underlying
reasoning (clients are expected to keep their own durable copy;
ClipHanger doesn't need to hold a result forever to be useful) still
applies for anyone who chooses to turn it on.

---

## Naming

**ClipHanger** — a wright makes things (playwright, shipwright); this
makes frames.

*Sprocket* was the other strong candidate and was **ruled out**: it is
already an open-source ffmpeg-based video editor
(github.com/SprocketVideo/Sprocket), a direct collision in the same
space. "Frame-" was preferred over "Still-" because the service produces
clips as well as stills.

Check Docker Hub and GHCR namespaces directly before publishing a first
image — search engines are a weak signal for registry availability, and
an image tag is hard to change once people have pulled it.

---

## Open questions

Not blockers, but each changes something structural if decided late.

- **Who initiates.** Only clients submit today. A scheduled or
  watch-folder mode could pre-generate a whole library overnight.
- **Multiple instances.** One is assumed. More would need clients to
  route jobs, which the current shape doesn't express.
- **Transport security.** Plain HTTP on a LAN carrying an API key.
  Acceptable, but it should be a recorded choice rather than an
  accident.
