// Package extract wraps ffmpeg and ffprobe — the only package that
// actually shells out. Every exported function takes the REAL source
// URL (needed to do the work) but scrubs it out of anything returned as
// an error, per CLAUDE.md's "never let a credential reach a log or an
// error string" rule: a Plex token or Kodi basic-auth credential lives
// right inside that URL, and ffmpeg happily echoes it into stderr on
// failure.
package extract

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"

	"github.com/srinath/framewright/internal/model"
)

// Source is what a backend.ResolvedSource looks like from this
// package's side — extract doesn't import package backend (nothing
// backend-specific belongs this deep; see that package's own header),
// so cmd/framewright/main.go converts between the two shapes at the
// one call site that has both in scope.
type Source struct {
	URL            string
	RedactedURL    string
	ExtraInputArgs []string

	// LogSink, if set, receives a live copy of ffmpeg's stderr as it's
	// written — not just the tail kept for a failed job's Error field.
	// Added 2026-08-22 per direct request ("show the execution live log
	// also somewhere if user wants to see"): a job that's merely slow
	// (a big MKV seeking over a home LAN, say) looks identical to a
	// stuck one from the outside until something like this exists.
	// package queue owns the actual buffer (see its livelog.go) and
	// passes its writer in here per job; this package doesn't know or
	// care that it's a ring buffer, only that it's an io.Writer.
	LogSink io.Writer
}

// scrub replaces every occurrence of the real URL with its redacted
// form in ffmpeg/ffprobe's output before it's ever stored or returned —
// belt-and-braces alongside backend.ResolvedSource.Redacted() already
// building a credential-free string; this is what actually keeps that
// string out of stderr specifically.
func (s Source) scrub(text string) string {
	if s.RedactedURL == "" {
		return text
	}
	return strings.ReplaceAll(text, s.URL, s.RedactedURL)
}

func (s Source) inputArgs(seekSeconds int) []string {
	// -ss BEFORE -i is load-bearing, not stylistic (docs/SERVER-NOTES.md):
	// placed here it becomes an INPUT seek, so ffmpeg issues an HTTP
	// range request and jumps straight to the timestamp rather than
	// decoding from the start of the file. For a scene 95 minutes in,
	// that's the difference between a couple of seconds and several
	// minutes of pointless transfer.
	args := []string{"-ss", strconv.Itoa(seekSeconds)}
	args = append(args, s.ExtraInputArgs...)
	args = append(args, "-i", s.URL)
	return args
}

// probeInputArgs is inputArgs WITHOUT -ss (bug found via a local smoke
// test, not caught by reading docs alone): ffprobe doesn't support -ss
// as a seek option the way ffmpeg does — it just fails outright
// ("Option not found"). That's fine: probing reads FILE-level metadata
// (duration, codecs, HDR, Atmos), which doesn't depend on where in the
// file you look, so there was never a reason to seek here in the first
// place.
func (s Source) probeInputArgs() []string {
	args := append([]string{}, s.ExtraInputArgs...)
	return append(args, "-i", s.URL)
}

// lastLines keeps only the tail of ffmpeg's stderr for a job's Error
// field — docs/API.md: "capture the last few lines of ffmpeg's stderr...
// that's almost always the actionable part." The full output is
// noisy (codec probing, every frame timestamp at higher log levels);
// the actual failure reason is almost always in the final handful of
// lines.
func lastLines(text string, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func run(ctx context.Context, name string, args ...string) (stdout, stderr string, err error) {
	return runLogged(ctx, nil, name, args...)
}

// runLogged is run() plus an optional live sink — os/exec copies from
// the process's stderr pipe to cmd.Stderr AS THE PROCESS WRITES, not
// just at exit, so a caller watching sink sees output in real time,
// same as watching a terminal.
func runLogged(ctx context.Context, sink io.Writer, name string, args ...string) (stdout, stderr string, err error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	if sink != nil {
		cmd.Stderr = io.MultiWriter(&errBuf, sink)
	} else {
		cmd.Stderr = &errBuf
	}
	err = cmd.Run()
	return outBuf.String(), errBuf.String(), err
}

// ffmpegError turns a failed run() into the error a job's Error field
// actually shows. Checks ctx.Err() FIRST (bug fixed 2026-08-22, found
// via a real report): when the job's own timeout kills ffmpeg mid-run
// via exec.CommandContext, whatever happened to be in stderr right
// before the kill is just NORMAL ffmpeg progress chatter — codec/stream
// info, filter warnings — not an error message at all, and showing it
// as one is actively misleading (it read as a real ffmpeg parse
// failure when the process was actually still working and just ran out
// of time). A genuine ffmpeg failure — bad seek, unsupported codec,
// unreachable source — still shows the real stderr tail exactly as
// before; only the timeout case gets its own, honest message.
func ffmpegError(ctx context.Context, operation, stderr string, scrub func(string) string) error {
	if ctx.Err() != nil {
		return fmt.Errorf(
			"%s timed out before finishing — the source may be slow to seek over the network (common for MKV, whose seek index isn't always positioned as conveniently for a remote byte-range seek as MP4's is), or the media server itself may be under load. Raise JOB_TIMEOUT_SECONDS if this happens consistently on your setup",
			operation,
		)
	}
	return fmt.Errorf("%s failed: %s", operation, scrub(lastLines(stderr, 12)))
}

// Still writes a single JPEG frame to outPath — docs/DECISIONS.md's
// fixed output spec: up to 1280px wide, no audio.
func Still(ctx context.Context, src Source, atSeconds int, outPath string) error {
	args := src.inputArgs(atSeconds)
	args = append(args,
		"-frames:v", "1", "-an",
		"-vf", "scale='min(1280,iw)':-2",
		"-q:v", "3",
		"-y", outPath,
	)
	_, stderr, err := runLogged(ctx, src.LogSink, "ffmpeg", args...)
	if err != nil {
		return ffmpegError(ctx, "ffmpeg still extraction", stderr, src.scrub)
	}
	return nil
}

// ClipResult reports what actually got written, so the caller can
// enforce the "one frame is a failure" rule (docs/API.md, CLAUDE.md) —
// this package does the counting; the caller (package queue) decides
// what a FrameCount of 1 means for job state, since that policy belongs
// with the thing that owns Job.State, not with the ffmpeg wrapper.
type ClipResult struct {
	FrameCount int
	Bytes      int
}

// Clip writes an H.264 MP4 to outPath — changed from an animated GIF
// (2026-08-23, per direct request, after a real duration/size
// conversation): a client wanted "60 seconds of the scene, not the
// smoothest playback" as the actual target, and GIF's per-frame
// palette+LZW encoding has no inter-frame compression at all, so its
// size scales almost linearly with spanSeconds × fps — a 60s clip would
// have run 20-30MB even at a modest frame rate. Real video compression
// (H.264 only encodes what CHANGES between frames) makes a 60-second
// clip dramatically smaller than the equivalent GIF, at better quality,
// which is why this replaces GIF outright rather than sitting alongside
// it — see docs/DECISIONS.md's "Output is fixed" entry for the full
// reasoning and the numbers that drove this.
//
// -movflags +faststart moves the MP4 metadata (moov atom) to the front
// of the file so a client can start playing before the whole file has
// downloaded — matters more here than it would for a 3-second clip, now
// that a "clip" can genuinely be a minute long. -pix_fmt yuv420p forces
// standard 8-bit output regardless of the source's own bit depth/color
// space (a source could be 10-bit HDR) — guarantees playback on
// anything that can decode H.264 at all, same "flatten for universal
// compatibility" reasoning the GIF path already applied.
func Clip(ctx context.Context, src Source, atSeconds, spanSeconds, fps int, outPath string) (ClipResult, error) {
	args := src.inputArgs(atSeconds)
	filter := fmt.Sprintf("fps=%d,scale='min(480,iw)':-2:flags=lanczos", fps)
	args = append(args,
		"-t", strconv.Itoa(spanSeconds), "-an",
		"-vf", filter,
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-crf", "23",
		"-pix_fmt", "yuv420p",
		"-movflags", "+faststart",
		"-y", outPath,
	)
	_, stderr, err := runLogged(ctx, src.LogSink, "ffmpeg", args...)
	if err != nil {
		return ClipResult{}, ffmpegError(ctx, "ffmpeg clip extraction", stderr, src.scrub)
	}

	frames, size, err := probeClip(ctx, outPath)
	if err != nil {
		// The file was written but couldn't be counted/measured — worth
		// surfacing distinctly from an ffmpeg failure, since the file
		// DOES exist at this point.
		return ClipResult{}, fmt.Errorf("clip was written but couldn't be inspected afterward: %w", err)
	}
	return ClipResult{FrameCount: frames, Bytes: size}, nil
}

// probeClip counts frames and reports file size via ffprobe — the
// authoritative source for FrameCount rather than trusting ffmpeg's own
// stdout, which doesn't reliably report this. Same technique regardless
// of container/codec, so this needed no changes moving from GIF to MP4
// beyond the name (it was probeGIF).
func probeClip(ctx context.Context, path string) (frameCount int, byteSize int, err error) {
	stdout, stderr, err := run(ctx, "ffprobe",
		"-v", "error",
		"-count_frames",
		"-select_streams", "v:0",
		"-show_entries", "stream=nb_read_frames",
		"-of", "csv=p=0",
		path,
	)
	if err != nil {
		return 0, 0, fmt.Errorf("ffprobe frame count failed: %s", lastLines(stderr, 8))
	}
	frameCount, convErr := strconv.Atoi(strings.TrimSpace(stdout))
	if convErr != nil {
		return 0, 0, fmt.Errorf("unexpected ffprobe frame-count output %q", stdout)
	}

	sizeOut, stderr2, err := run(ctx, "ffprobe", "-v", "error", "-show_entries", "format=size", "-of", "csv=p=0", path)
	if err != nil {
		return frameCount, 0, fmt.Errorf("ffprobe size check failed: %s", lastLines(stderr2, 8))
	}
	byteSize, _ = strconv.Atoi(strings.TrimSpace(sizeOut))
	return frameCount, byteSize, nil
}

// --- ffprobe media info ---

// probeStream/probeFormat mirror only the ffprobe JSON fields this
// package actually reads — see docs/SERVER-NOTES.md for the derivation
// rules each field below feeds.
type probeOutput struct {
	Format struct {
		Duration   string `json:"duration"`
		FormatName string `json:"format_name"`
	} `json:"format"`
	Streams []probeStream `json:"streams"`
}

type probeStream struct {
	CodecType        string `json:"codec_type"`
	CodecName        string `json:"codec_name"`
	Profile          string `json:"profile"`
	Width            int    `json:"width"`
	Height           int    `json:"height"`
	BitsPerRawSample string `json:"bits_per_raw_sample"`
	RFrameRate       string `json:"r_frame_rate"`
	ColorTransfer    string `json:"color_transfer"`
	Channels         int    `json:"channels"`
	ChannelLayout    string `json:"channel_layout"`
	Disposition      struct {
		Default int `json:"default"`
	} `json:"disposition"`
	SideDataList []struct {
		SideDataType string `json:"side_data_type"`
		DVProfile    int    `json:"dv_profile"`
		DVLevel      int    `json:"dv_level"`
	} `json:"side_data_list"`
}

// Probe runs ffprobe against src and derives DemoFlex/Framewright's own
// MediaInfo shape from the raw output — this is "better data than any
// server API gives" (SERVER-NOTES.md), which is the whole reason this
// exists rather than trusting whatever the media server itself reports.
// No seek (see probeInputArgs) — probing reads FILE-level metadata,
// which doesn't vary by position within one file.
func Probe(ctx context.Context, src Source) (*model.MediaInfo, error) {
	args := src.probeInputArgs()
	args = append(args, "-v", "error", "-print_format", "json", "-show_format", "-show_streams")
	stdout, stderr, err := run(ctx, "ffprobe", args...)
	if err != nil {
		return nil, ffmpegError(ctx, "ffprobe", stderr, src.scrub)
	}

	var parsed probeOutput
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		return nil, fmt.Errorf("decoding ffprobe output: %w — output: %s", err, truncate(stdout, 500))
	}

	info := &model.MediaInfo{Container: parsed.Format.FormatName}
	if durSeconds, convErr := strconv.ParseFloat(parsed.Format.Duration, 64); convErr == nil {
		info.DurationMs = int64(durSeconds * 1000)
	}

	for _, stream := range parsed.Streams {
		switch stream.CodecType {
		case "video":
			if info.Video.Codec != "" {
				continue // probe the FIRST video stream only, deliberately.
			}
			info.Video = deriveVideoInfo(stream)
		case "audio":
			// Probe every audio track, but record which is Default —
			// SERVER-NOTES.md: "reporting Atmos from track 3 while the
			// default track is stereo is worse than reporting nothing."
			info.Audio = append(info.Audio, deriveAudioInfo(stream))
		}
	}
	return info, nil
}

func deriveVideoInfo(s probeStream) model.VideoInfo {
	v := model.VideoInfo{
		Codec:     s.CodecName,
		Width:     s.Width,
		Height:    s.Height,
		FrameRate: s.RFrameRate,
	}
	if bd, err := strconv.Atoi(s.BitsPerRawSample); err == nil {
		v.BitDepth = bd
	}
	switch s.ColorTransfer {
	case "smpte2084":
		v.HDR = "HDR10"
	case "arib-std-b67":
		v.HDR = "HLG"
	}
	for _, sd := range s.SideDataList {
		if sd.SideDataType == "DOVI configuration record" {
			v.DolbyVision = &model.DolbyVision{Profile: sd.DVProfile, Level: sd.DVLevel}
		}
	}
	return v
}

func deriveAudioInfo(s probeStream) model.AudioInfo {
	a := model.AudioInfo{
		Codec:         s.CodecName,
		Profile:       s.Profile,
		Channels:      s.Channels,
		ChannelLayout: s.ChannelLayout,
		Default:       s.Disposition.Default == 1,
	}
	// Atmos/DTS:X have no reliable dedicated field anywhere (confirmed,
	// SERVER-NOTES.md) — a substring match on `profile` is the actual
	// state of the art, not a shortcut. ffmpeg reports e.g. "Dolby
	// TrueHD + Dolby Atmos".
	switch {
	case strings.Contains(a.Profile, "Atmos"):
		a.Spatial = "atmos"
	case strings.Contains(a.Profile, "DTS:X"):
		a.Spatial = "dtsx"
	}
	return a
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Version returns ffmpeg's own version string, for GET /health.
func Version(ctx context.Context) string {
	stdout, _, err := run(ctx, "ffmpeg", "-version")
	if err != nil {
		return ""
	}
	firstLine := strings.SplitN(stdout, "\n", 2)[0]
	// "ffmpeg version 7.1 Copyright..." → "7.1"
	fields := strings.Fields(firstLine)
	for i, f := range fields {
		if f == "version" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return firstLine
}
