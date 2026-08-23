# Server notes

Real API shapes and traps for the three backends. Researched August
2026. **Confidence is marked per item** — verify anything marked
UNVERIFIED against a live server before trusting it, and surface the raw
response body when a decode fails rather than a bare error.

---

## Resolving a streamable URL

This is the only part of Framewright that knows which server it's
talking to. Everything downstream sees a URL.

### Plex — CONFIRMED against a live server

```
GET http://HOST:32400/library/metadata/{itemId}?X-Plex-Token=TOKEN
    Accept: application/json
→ MediaContainer.Metadata[0].Media[0].Part[0].key
```

The `key` is an already-relative path. Build:

```
http://HOST:32400{key}?X-Plex-Token=TOKEN
```

**Trap:** `ratingKey` comes back as a JSON **string** in some responses
and an **Int** in others. Accept either. This was confirmed the hard way.

**Trap:** `/clients` does NOT reliably support `Accept: application/json`
across server versions and must be parsed as XML. Other endpoints are
fine with JSON.

### Kodi — UNVERIFIED

Kodi's files are usually on NFS/SMB shares, so serve them back through
Kodi's own webserver VFS endpoint.

```
POST http://HOST:8080/jsonrpc
  {"jsonrpc":"2.0","id":1,"method":"VideoLibrary.GetMovieDetails",
   "params":{"movieid":512,"properties":["file"]}}
→ "nfs://192.168.1.10/volume1/Movies/Film (2024)/film.mkv"

http://HOST:8080/vfs/{percent-encoded file path}
```

**Three failure modes worth handling explicitly:**

1. The webserver must be enabled (*Settings → Services → Control → Allow
   remote control via HTTP*).
2. Since Frodo, only paths inside a configured **source** are served. A
   file Kodi can *play* is not necessarily a file Kodi will *serve*.
3. A path outside a source returns **401, not 404** — which reads as an
   auth problem and sends you chasing the wrong thing. Say so in the job
   error rather than surfacing a bare 401.

**Performance:** bytes travel NAS → Kodi box → Framewright. If the Kodi
box is a Shield on wireless, that hop is the bottleneck, not ffmpeg.

Auth is HTTP Basic, optional. `http://user:pass@HOST:8080/vfs/...` works
with ffmpeg.

**Local path fallback for failure mode #3 (2026-08-22):** if a server
has `LocalPathFrom`/`LocalPathTo` configured (Setup page, Kodi section)
and the VFS HEAD check confirms a 401, `Resolve()` translates the raw
`file` path Kodi reported through that prefix mapping and reads it
directly instead of failing the job — see `backend.LocalPath()` and
`kodi.go`. Only engages on a confirmed 401 for that specific item;
everything else about Kodi resolution is unchanged. See
`docs/DECISIONS.md`'s "Media is streamed over HTTP" section for why
this stays narrow rather than becoming a general local-read mode.

### Jellyfin — UNVERIFIED

One call, no lookup:

```
GET http://HOST:8096/Items/{itemId}/Download
    Authorization: MediaBrowser Token=API_KEY
```

The credential is a **header**, not a query parameter, so it must be
passed to ffmpeg/ffprobe with `-headers` placed *before* `-i`.

---

## ffmpeg invocation

**Keep `-ss` before `-i`.** That makes it an *input* seek, so ffmpeg
issues an HTTP range request and jumps straight to the timestamp. Placed
after `-i` it decodes from the start of the file — for a scene 95
minutes in, that's the difference between a couple of seconds and
several minutes of pointless transfer.

### Still

```
ffmpeg -ss 5700 -i "$SRC" \
  -frames:v 1 -an \
  -vf "scale='min(1280,iw)':-2" \
  -q:v 3 still.jpg
```

### Clip

```
ffmpeg -ss 5700 -i "$SRC" \
  -t 3 -an \
  -vf "fps=10,scale=480:-2:flags=lanczos" \
  -loop 0 clip.gif
```

**No custom palette — confirmed 2026-08-22, and this is load-bearing,
not a shortcut.** An earlier version of this command used the standard
two-pass `palettegen`/`paletteuse` trick (`split` into two branches,
`palettegen=stats_mode=diff` builds a content-optimized 256-colour
palette, `paletteuse` applies it) specifically because GIF's 256-colour
limit wrecks film grain and dark scenes under ffmpeg's plain default
quantization. That reasoning was correct, but it hid a much bigger
problem: `palettegen`'s cost scales badly — apparently worse than
linearly — with the number of *distinct* input colors, and a grainy
source (a real MakeMKV Blu-ray rip, tested directly against the file
with no network involved) pushed a 3-second/30-frame sample past
400,000 distinct colors, turning sub-second work into **11+ minutes**
for one clip. Confirmed by elimination, ruling out one variable at a
time against the real file: not HTTP/Plex serving (reproduced with the
exact same slowness reading the file directly off disk), not seek depth
(a single-frame grab at the same deep timestamp took 0.53s), not just
`stats_mode=diff` specifically (removing it was still ~90s+ for 30
frames), not the `split`-based single-command structure (two fully
separate `palettegen`-then-`paletteuse` passes were also slow). Dropping
the custom palette entirely and using ffmpeg's built-in default
(unconditionally, not just as a slow-path fallback) fixed it: same file,
same timestamp, 0.34s. The trade-off is real — flatter colour, visible
banding in dark/grainy scenes — and was accepted deliberately for this
use case (a demo-scene preview GIF, not an archival transcode) after
checking the actual output looked acceptable. Don't reintroduce
palettegen/paletteuse without a real plan for its cost blowing up on
grainy sources.

**Concurrency:** cap parallel ffmpeg processes at 2–4. Each is a full
decode, and the same box may be serving media at the same time.

---

## Media info via ffprobe

Framewright has the file open anyway, so probing is nearly free — and it
is **better data than any server API gives**.

```
ffprobe -v error -print_format json -show_format -show_streams "$SRC"
```

Derivations:

| Property | Rule |
|---|---|
| HDR10 | `color_transfer == "smpte2084"` |
| HLG | `color_transfer == "arib-std-b67"` |
| Dolby Vision | `side_data_list` entry of type `DOVI configuration record`, carrying `dv_profile` and `dv_level` |
| Atmos | audio `profile` contains `"Atmos"` (ffmpeg reports e.g. `Dolby TrueHD + Dolby Atmos`) |
| DTS:X | audio `profile` contains `"DTS:X"` |
| 4K | `width >= 3840` |

**Test 4K on WIDTH, not height.** Scope-framed 4K is commonly
3840×1600; a `height >= 2160` test silently drops it.

**Probe the same part you extract from.** A title can have several parts
and several audio tracks; reporting Atmos from track 3 while the default
track is stereo is worse than reporting nothing. Record which track the
flags came from.

`format.duration` gives exact millisecond duration — more precise than
Plex reports, and the reliable way to tell two cuts or editions apart.

---

## What the servers themselves expose

For reference — this is what a client would get *without* Framewright,
and why ffprobe is worth doing.

| | Resolution / HDR | Channels | Atmos / DTS:X |
|---|---|---|---|
| **Plex** | `videoResolution`, `bitDepth`, `colorTrc`, explicit `DOVIPresent`/`DOVIProfile` | `channels`, `audioChannelLayout` (`"5.1(side)"`) | **No field.** Substring guess only |
| **Kodi** | `width`/`height`, `hdrtype` (first-class) | `channels` only, no layout | Sometimes in codec (`truehd_atmos`), unreliable |
| **Jellyfin** | `VideoRange`, `VideoRangeType`, `BitDepth` | `Channels`, `ChannelLayout` | `AudioSpatialFormat` enum |
| **ffprobe** | all of the above, plus DV profile *and* level | full per-track detail | ground truth via `profile` |

**Atmos has no reliable API anywhere.** Jellyfin's `AudioSpatialFormat`
is literally `Profile.Contains("Dolby Atmos")` — every client ends up
doing the same substring match. This is not a hack; it's the state of
the art.

**Kodi's HDR key casing is inconsistent** between JSON-RPC and NFO
(`hdrtype` vs `hdrType`, xbmc/xbmc#22169). Accept both spellings.
