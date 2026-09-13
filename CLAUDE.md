# CLAUDE.md — ClipHanger

Persistent context for whoever (human or model) works on this. The
service is built and running in production (a real Synology NAS
deployment, see `docs/DECISIONS.md`'s deployment notes) — this file is
ground rules for extending it, not a design handoff for starting from
scratch.

Read `docs/API.md` (the contract), `docs/DECISIONS.md` (what is already
settled and why) and `docs/SERVER-NOTES.md` (real API shapes and traps)
before writing code.

## What this is

ClipHanger is a **self-hosted service that extracts stills and short
clips from a media library at a given timestamp.** A client says "give
me a still and a 3-second clip of item X at 1:35:00"; ClipHanger asks
the media server where that file lives, hands the URL to ffmpeg, and
keeps the results until the client collects them.

It talks to **Plex, Kodi and Jellyfin**. None of them is privileged — a
Kodi-only or Jellyfin-only library must be as well served as a Plex one.

## Why it exists

The clients that most want this can't do it themselves. The motivating
case is iOS: AVFoundation has **no Matroska support**, so an `.mkv`
never decodes on an iPhone regardless of its codecs — and a home media
library is mostly `.mkv`. Asking the server to transcode to HLS works
around the container, but `AVAssetImageGenerator` doesn't operate on
HLS, so frames have to be scraped out of a live player: slow, fragile,
and it burns server CPU every time. ffmpeg on a always-on box has none
of these problems.

## It is general-purpose

It is being open-sourced. **Nothing about any particular client may leak
into the API.** The first consumer is DemoFlex (an iOS app that launches
demo-worthy movie scenes on a home theatre), but a reader of this repo
should never need to know that. Concretely: jobs are keyed by a
client-chosen opaque `captureId`, not by anything with app semantics.

## Hard rules

- **Never read media off disk speculatively, or as a hidden fallback
  inside another backend.** Always stream from the media server over
  HTTP by default — this is what lets the service work regardless of
  which machine it runs on. Direct disk access is opt-in, and comes in
  two deliberately different shapes, neither of which is "try it just
  in case":
  - **Kodi's own narrow rescue** (2026-08-22, unchanged): a Kodi server
    can optionally configure `LocalPathFrom`/`LocalPathTo`, used ONLY
    after confirming Kodi's own HTTP path doesn't work for a specific
    item (its VFS 401-outside-a-source failure — see
    `internal/backend/kodi.go`). Never wire a silent fallback like this
    into a NEW backend without an equally real, confirmed failure
    condition of its own — Plex and Jellyfin still don't have this, and
    still shouldn't get it "just in case."
  - **`KindLocal`, a dedicated server kind** (2026-09-12, direct
    request): the user explicitly adds a "local mount" server — no
    host, no credentials, just a path-prefix mapping to a disk
    ClipHanger already has access to — and a client (DemoFlex) chooses
    to resolve a specific item through it, same as choosing any other
    server. This is NOT the same thing as the bullet above: nothing
    engages it automatically or silently. See
    `internal/backend/local.go`.
- **Credentials never travel from a client.** Media-server credentials
  are entered in ClipHanger's own web UI and stay on the box. Clients
  hold only an API key. There is deliberately no client-facing endpoint
  to set them.
- **Never let a credential reach a log or an error string.** Plex tokens
  live inside the source URL, so they land in ffmpeg's stderr by
  default. Scrub before storing or returning anything.
- **Don't bundle ffmpeg.** Install it from the distro in the image and
  treat it as an external dependency. Redistributing ffmpeg builds
  carries GPL/LGPL obligations that vary with build configuration.
- **A capture that yields one frame is a FAILURE, not a success.** The
  caller asked for motion. Reporting it as done is how a units bug hid
  for a whole round of testing during design.

## Working style

- Small, verifiable increments. Get one backend resolving a URL before
  adding the next; get extraction working before building the web UI.
- **Verify API shapes against a real server rather than assuming.** The
  Plex/Kodi/Jellyfin shapes in `docs/SERVER-NOTES.md` were researched,
  but only some were confirmed against live servers — each is marked.
  When a response doesn't decode, surface the raw body in the error
  rather than a bare decode failure. That practice caught several wrong
  assumptions during DemoFlex's development.
- Flag clearly when something can't be verified without hardware the
  environment doesn't have.
- Ask before expanding scope beyond `docs/API.md`.
