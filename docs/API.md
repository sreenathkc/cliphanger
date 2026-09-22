# API contract

Base URL `http://<host>:<port>`. JSON in, JSON out, except media
responses. **Implemented and tested against a real Plex library** — see
the top-level README for current status of the Kodi/Jellyfin backends.

Every request carries `X-Api-Key: <shared secret>`. Anything without it
gets `401` and no body. The key is generated on the box, shown once in
the web UI, and typed into clients.

---

## Lifecycle

These four steps are a real sequence; each depends on the one before.

1. **Configure once, in the web UI.** Media-server credentials are
   entered on the service's own setup page and stay there. Clients never
   see or send them.
2. **Submit a batch.** A client posts captures and gets an immediate
   acknowledgement. Nothing blocks.
3. **Poll for state.** One request covers the whole queue.
4. **Fetch lazily.** Media is pulled per capture when first needed.
   ClipHanger is the store of record.

---

## Endpoints

### `GET /health`

Liveness plus enough state for a dashboard without a second call.

```json
{
  "service": "cliphanger",
  "version": "0.1.0",
  "ffmpeg": "7.1",
  "servers": 2,
  "queued": 12,
  "running": 2,
  "failed": 1
}
```

### `GET /servers`

Configured media servers, so a client can let the user pick one (or
match by kind) when submitting. **Read-only and credential-free.**
Adding, editing and removing servers happens in the web UI; there is
deliberately no client-facing endpoint for it.

```json
{
  "servers": [
    {
      "serverId": "srv_7f3a",
      "kind": "kodi",
      "name": "Living Room Shield",
      "reachable": true
    }
  ]
}
```

`kind` is one of `plex`, `kodi`, `jellyfin`, `local`.

`local` (2026-09-12) isn't a media server at all — it's a direct disk
mount the admin configured in Setup (a path-prefix mapping, no
host/credentials). Resolving an item through it means submitting the
raw file path your OWN source (Plex/Kodi/Jellyfin) reports for that
item as `source.itemId`, instead of that source's own opaque id — see
`docs/DECISIONS.md`'s "Media is streamed over HTTP by default, direct
disk access is opt-in" for why this exists and when it's actually worth
trying (typically: as a fallback after resolving through the item's own
real server fails, not as the first attempt).

### `POST /jobs`

Submit a batch. Idempotent by `captureId` — resubmitting something
already done is skipped rather than redone, unless `force` is set.

```json
{
  "force": false,
  "jobs": [
    {
      "captureId": "any-stable-client-chosen-string",
      "source": { "serverId": "srv_7f3a", "itemId": "512" },
      "timestampSeconds": 5700,
      "spanSeconds": 20,
      "fps": 10
    }
  ]
}
```

`itemId` is that server's own id: Plex `ratingKey`, Kodi `movieid`,
Jellyfin item GUID.

→ `202`

```json
{
  "accepted": 1,
  "skipped": 0,
  "jobs": [{ "captureId": "...", "state": "queued" }]
}
```

### `GET /jobs`

Whole queue. Optional `?state=failed`, `?since=<iso8601>`.

```json
{
  "jobs": [
    {
      "captureId": "...",
      "state": "done",
      "frameCount": 200,
      "stillBytes": 142880,
      "clipBytes": 486211,
      "updatedAt": "2026-08-21T18:04:11Z",
      "error": null,
      "mediaInfo": { }
    }
  ]
}
```

### `GET /jobs/{captureId}`

One job. `captureId` is client-chosen and may contain anything, so it
must be percent-encoded in the path.

### `GET /media/{captureId}/still` · `GET /media/{captureId}/clip`

The artifacts, as `image/jpeg` and `video/mp4`. `404` unless the job is
`done`. Support `ETag` / `If-None-Match` so clients can re-check cheaply.

### `DELETE /jobs/{captureId}`

Drops the job and its media so it can be regenerated — the escape hatch
for a bad timestamp or a capture that came out badly.

---

## Job model

States: `queued` · `running` · `done` · `failed`

| Field | Type | Notes |
|---|---|---|
| `captureId` | string | Primary key, chosen by the CLIENT and opaque here. Any stable string it can regenerate. Doubles as the idempotency key. |
| `source.serverId` | string | Which configured server holds this item. From `GET /servers`. |
| `source.itemId` | string | That server's own id. Resolved to a URL per backend. |
| `timestampSeconds` | int | Where the capture starts. Seconds, not milliseconds. |
| `spanSeconds` | int | Clip length. Default 20. |
| `fps` | int | Clip frame rate. Default 10. |
| `speedMultiplier` | int? | Reads `spanSeconds × speedMultiplier` of source, time-compressed back down to `spanSeconds` — covers more of a longer scene without a longer clip. Default 2, Setup-page-only in practice: DemoFlex doesn't send this per job (unlike `spanSeconds`), so the server's own configured default governs real submissions. |
| `resolvedSource` | string? | The actual file/URL `source.itemId` resolved to, already credential-redacted. Set once resolution succeeds — present on a `done` job, and on a `failed` one too if the failure happened after resolving (an ffmpeg error, say) rather than during it (an unknown item, a server that's gone). Absent while still `queued`. |
| `title` | string? | The item's own display title, straight from the media server (2026-09-22) — same availability as `resolvedSource` above. Best-effort: absent when the backend couldn't get one, never a reason to fail the job. |
| `state` | enum | One of the four above. |
| `error` | string? | Human-readable reason when `failed`. Null otherwise. |
| `frameCount` | int? | Frames actually written. **1 means nothing animates — treat as failure.** |
| `stillBytes` | int? | Size of the generated still, once `done`. |
| `clipBytes` | int? | Size of the generated clip, once `done`. |
| `mediaInfo` | object? | ffprobe results, below. |
| `updatedAt` | ISO 8601 | Drives `?since=` polling. |

---

## Output spec

Fixed by the service so clients never negotiate formats. Was a GIF
until 2026-08-23 — see `docs/DECISIONS.md` for why that changed to a
real video: GIF has no inter-frame compression, so its size scales
almost linearly with `spanSeconds × fps`, which stopped being viable
once a clip needed to run longer than a few seconds.

| | Still | Clip |
|---|---|---|
| Format | JPEG | MP4 (H.264, `yuv420p`, faststart) |
| Width | up to 1280px | up to 480px |
| Duration | single frame | `spanSeconds` (default 20) |
| Frame rate | — | `fps` (default 10) |
| Audio | — | stripped |
| Typical size | ~150 KB | ~`spanSeconds` × 25 KB (constant bitrate, see below) |

Clip size targets a fixed **bitrate** (200 kbps), not a fixed quality,
as of 2026-08-24 — changed from CRF per direct request ("a 1080 movie
and a 4k UHD HDR movie... ideally both format should generate same file
output"). CRF spends however many bits a scene actually needs, so size
scaled with content complexity regardless of source resolution/HDR (both
already normalized away by the fixed 480px width and 8-bit output) —
real evidence: two SDR sources at identical settings on the same box
produced 186829 and 1194204 bytes, a 6x spread, from content alone.
Constant bitrate makes every clip converge on roughly the same size
instead, trading some quality consistency (a busy scene now compresses
softer rather than growing the file) for size predictability.

---

## Media info

Returned on the job. See `SERVER-NOTES.md` for the ffprobe invocation
and the derivation rules.

```json
{
  "durationMs": 8296320,
  "container": "matroska,webm",
  "video": {
    "codec": "hevc",
    "width": 3840,
    "height": 1600,
    "bitDepth": 10,
    "frameRate": "23.976",
    "hdr": "HDR10",
    "dolbyVision": { "profile": 8, "level": 6 }
  },
  "audio": [
    {
      "codec": "truehd",
      "profile": "Dolby TrueHD + Dolby Atmos",
      "channels": 8,
      "channelLayout": "7.1",
      "spatial": "atmos",
      "default": true
    }
  ]
}
```

---

## Failure handling

- Capture the last few lines of ffmpeg's stderr into `error`. That's
  almost always the actionable part.
- **Never let a credential reach the error string.** It appears in the
  source URL, so scrub before storing or returning.
- A job producing one frame is `failed`, not `done`.
- Retry transient network failures a couple of times with backoff. Do
  not retry a decode error — it will fail identically forever.
- Keep failed jobs in the queue. They're the whole reason the Jobs page
  exists.

---

## Web UI

Three pages, server-rendered, embedded in the binary.

| Page | What it does |
|---|---|
| **Setup** | Add, edit and remove media servers, each with a *Test connection* button that verifies before saving. Generate and rotate the API key. |
| **Jobs** | The queue, filterable by state. Failures show the captured ffmpeg error. Thumbnails of completed captures, with retry and delete. |
| **Health** | Service version, detected ffmpeg version, disk used by stored media, per-server reachability. |

Open by default on the LAN, no login (revised 2026-08-23 — see
docs/DECISIONS.md "Web UI is open by default, not Basic-Auth-walled").
It does manage media-server credentials, but the earlier "must be
behind at least the API key" stance made first login circular in
practice on a trusted home LAN.

---

## Deployment

See the top-level README's Quick start and `docker-compose.yml` for the
real, working compose file (host networking on Linux — see its own
comments for why: a bridge network's Docker-internal source IP gets
misclassified as WAN traffic by Plex and fast-rejected). No published
container image yet; build from source (`docker compose build` or the
Dockerfile directly) — multi-arch (`linux/amd64`, `linux/arm64`).
