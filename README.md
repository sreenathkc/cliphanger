# Framewright

Extract stills and short clips from your media library at a given
timestamp.

Point it at Plex, Kodi or Jellyfin, ask for "item X at 1:35:00", and get
back a still and a 20-second clip — plus what the file actually is (4K,
HDR10, Atmos, DTS:X). Self-hosted, single container, no cloud, no
accounts beyond your own media servers.

> **Status: early, working.** Core pipeline (server config, resolve,
> extract, queue, web UI, Sign in with Plex) runs end-to-end and has
> been tested against a real Plex library on a real NAS. Kodi and
> Jellyfin backends are implemented but not yet verified against real
> servers. No release builds or container images are published yet —
> build from source (below).

## Why

Plenty of clients want a frame from a specific moment and can't get one.
iOS is the motivating case: its media stack has no Matroska support, so
`.mkv` — most of a real media library — simply won't decode on-device,
and `AVAssetImageGenerator` doesn't work against a transcoded HLS stream
either. Browsers and embedded clients hit their own versions of the same
wall. ffmpeg on an always-on box has none of these problems.

Framewright does the work once, keeps the result, and hands it to
whichever client asked — without that client ever touching a media
server's credentials, a NAS mount, or a path translation.

## How it works

1. Configure your media servers in the web UI — "Sign in with Plex"
   discovers your Plex servers automatically (no host/port/token to type
   in by hand); Kodi and Jellyfin take a host and credentials directly.
   Credentials never leave the box.
2. A client submits a batch of captures (`POST /jobs`) and walks away.
3. Framewright asks the server where each file actually lives, streams
   it over HTTP, and runs ffmpeg — still + clip + an ffprobe pass, in
   one job.
4. The client polls (`GET /jobs`), then fetches the still/clip it wants.

It never reads media off disk directly — no NAS mounts, no path
translation, and it works regardless of which machine it runs on
relative to where the media actually lives.

## Features

- **Plex, Kodi, Jellyfin** — one job model across all three; each
  backend resolves its own server's item id to a real streamable URL.
- **Sign in with Plex** — the standard plex.tv PIN/link flow, followed
  by automatic server discovery (`GET /api/v2/resources`), preferring a
  server's LOCAL network address over a remote/relay one. No manual
  host/port/token entry needed for Plex.
- **Still + clip in one pass** — a JPEG at the exact timestamp, and an
  MP4 (H.264) clip spanning it (`spanSeconds`/`fps` are the only two
  adjustable knobs — see `docs/DECISIONS.md` "Output is fixed, not
  negotiated").
- **Real media metadata, not a guess** — an ffprobe pass alongside every
  job reports resolution, HDR10/HLG/Dolby Vision (with profile), and
  audio codec/channels/Atmos/DTS:X, so a client can show accurate format
  badges instead of inferring them from a filename.
- **Idempotent job submission** — resubmitting a `captureId` that's
  already `done` is skipped, not redone, unless `force` is set.
- **Live ffmpeg output on the Jobs page** — watch a running job's actual
  ffmpeg stderr while it's in flight, not just a spinner.
- **Configurable retention** — keeps generated media forever by default;
  set a retention window (in hours) from the Setup page at any time,
  takes effect immediately, no restart. A background sweep reaps
  expired jobs and their files.
- **Configurable concurrency and timeout** — `WORKERS` caps how many
  ffmpeg processes run at once (each is a full decode, so this really is
  a concurrency limit, not just a thread-pool size); `JOB_TIMEOUT_SECONDS`
  bounds how long one job may run before it's killed and marked failed
  (a slow remote seek over MKV can genuinely take minutes).
- **Open web UI, API-key-gated API** — the Setup/Jobs/Health pages have
  no login, matching how comparable self-hosted tools (Sonarr, Radarr,
  Overseerr) behave on a trusted home LAN; every `X-Api-Key`-gated
  endpoint is what an actual client (like DemoFlex) authenticates
  against. See `docs/DECISIONS.md`.
- **Credential-free `/servers`** — a client can list configured servers
  and pick one without ever seeing a Plex token or Kodi password.
- **Deploy script for a self-hosted NAS** — `deploy-to-nas.sh` ships
  source over SSH (not rsync — see its own comments for why) and runs
  `docker compose up -d --build` remotely, for a Synology-style box with
  no local Docker.

## Quick start

Clone the repo and run it with Docker Compose — no published image yet,
so this builds from source:

```bash
git clone https://github.com/sreenathkc/framewright.git
cd framewright
docker compose up -d --build
```

The shipped `docker-compose.yml` uses host networking on Linux
(`network_mode: host`) rather than a bridge + port mapping — see its own
comments for why: under a bridge network, Framewright's outbound
requests to a media server on the same LAN carry a Docker-internal
source IP, which Plex doesn't recognize as local and fast-rejects with a
500. Host networking makes Framewright indistinguishable from any other
process on the box. Docker Desktop on macOS/Windows doesn't support host
networking the same way — uncomment the `ports:` mapping in
`docker-compose.yml` there instead.

Then open `http://your-host:8420` — no login, add a media server (Sign
in with Plex, or enter Kodi/Jellyfin details directly), and you're done.
When a client (like DemoFlex) needs the API key, it's on the Setup page,
or via `docker logs <container> | grep "API key ready"`.

### Running without Docker

```bash
make build   # go build -o bin/framewright ./cmd/framewright
make run     # DATA_DIR=./data ./bin/framewright
```

ffmpeg must be installed and on `PATH` — Framewright shells out to it
rather than bundling it (redistributing ffmpeg builds carries GPL/LGPL
obligations that vary by build configuration; see `CLAUDE.md`).

### Deploying to a self-hosted NAS

`deploy-to-nas.sh` handles the common case (a Synology-style NAS with
Docker/Container Manager but no local build tooling and no `docker`
group): it tars the source over SSH and runs `docker compose up -d
--build` on the NAS itself. Point it at your box once via environment
variables in your own shell profile (not committed to this repo):

```bash
export FRAMEWRIGHT_NAS_REMOTE=you@192.168.1.50
export FRAMEWRIGHT_NAS_PATH=/volume1/docker/framewright   # optional, this is the default
./deploy-to-nas.sh
```

Read the script's own header comment for the one-time SSH key and
passwordless-sudo setup it expects on the NAS side.

### Environment variables

| Variable | Default | What it does |
|---|---|---|
| `DATA_DIR` | `/data` | Where the store file and generated media live. |
| `PORT` | `8420` | HTTP port. |
| `WORKERS` | `3` | Max simultaneous jobs — each worker runs exactly one ffmpeg process at a time, so this is the concurrency cap, not just a pool size. Clamped 1–4 (each is a full decode; the same box may be serving media at the same time). |
| `API_KEY` | *(generated)* | Seeds the API key on first run only. Rotate from the web UI afterwards — this variable is not re-applied on restart. |
| `JOB_TIMEOUT_SECONDS` | `300` | How long one job (resolve + still + clip + probe) may run before it's killed and marked failed. Raise this if your setup seeks slowly over the network — MKV in particular, whose seek index isn't always positioned as conveniently for a remote byte-range seek as MP4's is. |
| `RETENTION_HOURS` | `0` (forever) | Seeds the initial retention window in hours — how long a done/failed job's media and record stick around before an automatic sweep deletes them. Only takes effect once, on a fresh install; change it anytime afterward from the Setup page instead (takes effect immediately, no restart). `0` means keep forever. |

## API

Every client-facing endpoint sits at a bare path (`/health`, `/servers`,
`/jobs`, `/media/...`) and requires `X-Api-Key`. The web UI lives under
`/ui/` and is open by default — see `docs/API.md` for the full contract
and `docs/DECISIONS.md` for why those two things have different access
rules.

```bash
curl -H "X-Api-Key: $KEY" http://your-host:8420/health
```

## Documentation

| | |
|---|---|
| [`CLAUDE.md`](CLAUDE.md) | Context and hard rules for anyone (or any model) working on this |
| [`docs/API.md`](docs/API.md) | The HTTP contract — endpoints, job model, output spec |
| [`docs/DECISIONS.md`](docs/DECISIONS.md) | What's already settled, and the reasoning behind it |
| [`docs/SERVER-NOTES.md`](docs/SERVER-NOTES.md) | Real Plex/Kodi/Jellyfin API shapes, ffmpeg invocations, and the traps |

**Start with `CLAUDE.md`.** The rules there exist because breaking one
of them is expensive to undo.

## Clients

- **DemoFlex** — an iOS app for browsing demo-worthy movie scenes and
  playing them on a home theatre. The first consumer, and the reason
  this exists. Framewright knows nothing about it, and shouldn't — the
  API is generic, not shaped around any one client.

## Status / roadmap

- Plex: implemented and tested end-to-end against a real library,
  including "Sign in with Plex" and automatic server discovery.
- Kodi, Jellyfin: backends implemented (resolve + stream + metadata),
  not yet verified against a real server.
- No published container image (GHCR) yet — build from source.
- No release binaries yet.

## Licence

TBD before first public release. Note that ffmpeg is an external
dependency and is deliberately **not** redistributed — see
`docs/DECISIONS.md`.
