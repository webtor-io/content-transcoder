# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

HTTP stream to HLS (HTTP Live Streaming) transcoder. Accepts a source URL, transcodes it using FFmpeg, and serves the resulting HLS segments via an HTTP server. Part of the [webtor.io](https://webtor.io) ecosystem.

## Build & Run

```bash
# Build
go build -o server .

# Run locally (requires FFmpeg 3+ with libfdk_aac installed)
./server --player=true
# Then open http://localhost:8080/player/?source_url=https://example.com/video.mkv

# Docker build (multi-stage: golang → jrottenberg/ffmpeg:8-alpine)
docker build -t content-transcoder .
```

The `.env` file contains local development overrides. Tests exist in `services/*_test.go` — run with `go test ./services/ -v`.

## Documentation

Detailed architecture docs are in `docs/`:
- **[docs/session-transcoding.md](docs/session-transcoding.md)** — Session-based transcoding architecture: session API, shared runs, seek quantization, FFmpeg lifecycle. **Read this before modifying** `session.go`, `session_manager.go`, `transcode_run.go`, `run_manager.go`, or `web.go`.

## Architecture

### Service Initialization (configure.go)

All services are wired in `configure.go:run()`. The CLI framework is `urfave/cli` with flags registered per-service via `Register*Flags()` functions. Services implement `cs.Servable` from `github.com/webtor-io/common-services`.

### Session-Based Transcoding

The system uses a Plex-style session API. Each viewer creates a session, which manages one HLS stream with seek support via API calls.

```
Player                          Server
  |                               |
  +- POST /session?source_url=  --> Create session, probe media, start FFmpeg from 0
  |  <-- { id, duration }        |
  |                               |
  +- GET /session/{id}/index.m3u8 -> Master playlist (static)
  +- GET /session/{id}/v0.m3u8   -> Dynamic EVENT playlist (from FFmpeg .ffmpeg file)
  +- GET /session/{id}/v0-0.ts   -> Segment file (waits until available)
  |                               |
  |   [user seeks to 50%]         |
  +- POST /session/{id}/seek?t=  -> Stop old run, start new FFmpeg from seek position
  |  <-- { ok: true }            |
  |                               |
  +- DELETE /session/{id}        -> Stop FFmpeg, cleanup
```

### Shared Runs (TranscodeRun + RunManager)

Multiple sessions for the same source URL and seek position share a single FFmpeg process via `RunManager`:

- **TranscodeRun** (`transcode_run.go`) — One FFmpeg process writing to `{hashDir}/runs/seek-{time}/`. Reference-counted; cleanup after 30s grace period when last session detaches.
- **RunManager** (`run_manager.go`) — Pool of TranscodeRuns keyed by `(hashDir, seekTime)`. `Acquire()` returns existing or creates new; `Release()` decrements refcount.
- **Seek quantization** — Seek times are rounded to 30s boundaries so nearby seeks share a run.

### Key Services (services/)

- **Session** (`session.go`) — Thin wrapper: manages master playlist directory, delegates FFmpeg to a shared TranscodeRun via RunManager. Methods: `Start`, `Seek`, `Stop`, `Close`, `RestartForSegment`.
- **SessionManager** (`session_manager.go`) — Manages active sessions. Reaper goroutine releases runs after 60s inactivity, removes sessions after 10min.
- **TranscodeRun** (`transcode_run.go`) — Shared FFmpeg process with reference counting. Handles start/stop/cleanup.
- **RunManager** (`run_manager.go`) — Deduplicates FFmpeg processes. Reaper cleans up idle runs after grace period.
- **ContentProbe** (`content_prober.go`) — Probes media metadata via gRPC or local ffprobe. Caches in `index.json`.
- **HLSBuilder/HLS** (`hls.go`) — Generates FFmpeg arguments for HLS encoding. Builds master playlist with video, audio, and subtitle stream groups.
- **Audio outputs** (`audio.go`) — `audioOutputFor`, the one per-track decision (copy, AAC stereo, AAC 5.1 at 384k) that the argv, the seek cuts, the masters (CODECS, CHANNELS, BANDWIDTH) and the run variant read.
- **TouchMap** (`touch_map.go`) — Maintains `{hashDir}.touch` file for external cleanup processes.

### HTTP Routes (services/web.go)

Session API:
- `POST /session?source_url=` — Create session, probe, start FFmpeg, return `{id, duration}`; `decode=` declares what the client decodes: HEVC tokens (video route) and `aac51`, `ac3`, `ec3` (audio: AAC 5.1 copied or encoded instead of stereo, E-AC-3/AC-3 copied on fMP4). See docs/session-transcoding.md, Audio
- `POST /session/{id}/seek?t=` — Seek to position (quantized to 30s boundaries)
- `DELETE /session/{id}` — Close session
- `GET /session/{id}/index.m3u8` — Master playlist
- `GET /session/{id}/{stream}.m3u8` — Variant playlist (polls .ffmpeg file)
- `GET /session/{id}/{segment}.ts` — Segment file (auto-restarts FFmpeg if needed)
- `GET /session/{id}/{segment}.m4s`, `GET /session/{id}/{stream}-init-{gen}.mp4` — fMP4 segment and init of a passthrough session (HEVC as it is); 404 on the old route. See docs/session-transcoding.md, Passthrough output
- A session this pod does not hold (expired after 10 min idle, lost on a rollout): `404 session not found`. For a GET/HEAD of a playlist, segment or init, the 404 comes after `UNKNOWN_SESSION_DELAY` (2 s). This slows players that ask again the moment a 404 arrives (`services/unknown_session.go`, docs/session-transcoding.md, "Unknown session"). Seek, DELETE and 404s inside a live session are answered at once

Capabilities:
- `GET /capabilities` — `{"passthrough_video_codecs":[...]}`, what a session opened now passes through (the capability file re-read as for POST /session; `[]`, never null, when none). For services (web-ui's Discover), no CORS; no session, run or metric is touched. See docs/session-transcoding.md, Capabilities

Player:
- `GET /player/?source_url=` — Web player UI (when `--player=true`)

### FFmpeg Seek Strategy

Depends on codec mode:

| Mode | `-ss` position | Flags | Rationale |
|------|---------------|-------|-----------|
| **Copy** (h264 source) | Before `-i`; `-ss 0` on each subtitle output | `-ss T -noaccurate_seek -itsoffset T-R -i URL ... -ss 0 -map 0:<subtitle> ...` | Fast input-level seek; `-noaccurate_seek` starts both video and audio from same keyframe for A/V sync. R (the offset) is where FFmpeg's own seek lands (probed with FFmpeg, as passthrough; ffprobe's seek is not FFmpeg's), after T too (MPEG-TS). Every output counts from R, so cues + offset are movie time (a run landing at 0 serves the run from 0's subtitles byte for byte); video bytes unchanged, audio too unless its first packet after the seek comes after the keyframe (then it counts from R, as from 0). R after T: no `-itsoffset` (it would shift the audio late). No probe answer (failed, implausible): the input seek alone, no `-itsoffset`, no cut — R is not known, and a cut at T would drop the cues between the keyframe and T and run the rest early |
| **Re-encode** (mpeg4, vp9, etc.) | Before `-i`; `-ss 0` on each copied audio (AAC up to 2 channels, up to 6 with `aac51`) and each subtitle output | `-ss T -i URL ... -ss 0 -map 0:<copied audio> ... -ss 0 -map 0:<subtitle> ...` | Input seek to the keyframe before T, then accurate decode up to T; the accurate seek trims only what goes through a filter graph, so copied audio (A/V) and subtitles (cue times against the offset) are cut at T on their outputs. `-xerror` is dropped on seeks |
| **Passthrough** (HEVC → fMP4) | Before `-i`; `-ss 0` on each audio output (copied E-AC-3/AC-3 included) | `-ss T -noaccurate_seek -itsoffset T-R -i URL ... -ss 0 -map 0:<audio> ...` | The copy seek; R is where FFmpeg's own seek lands (probed with FFmpeg, not ffprobe), every output counts from it, subtitles included, and the audio before it is dropped (A/V). No R: no offset, no cut, and nothing remembered for the key |

### Output Directory Structure

```
{output}/
  {sha1_hash}/                    # Per-content directory (SHA1 of source URL path)
    {sha1_hash}.touch             # Access timestamp for external cleanup
    index.json                    # Cached probe result
    sessions/
      {sessionID}/
        index.m3u8                # Master playlist (static, per-session)
    runs/
      seek-0.000/                 # Shared run at position 0
        v0-720-0.ts               # Video segments
        a0-0.ts                   # Audio segments
        v0-720.m3u8.ffmpeg        # FFmpeg's raw playlist output
        ffmpeg.out, ffmpeg.err    # FFmpeg logs
      seek-300.000/               # Shared run at position 300s
        ...
      a6c-seek-0.000/             # Runs of a declaration that changes the audio (hevc-seek-*: passthrough)
```

## Key Configuration (env vars / CLI flags)

| Flag | Env Var | Default | Purpose |
|------|---------|---------|---------|
| `--output` / `-o` | `OUTPUT` | `out` | Output directory (supports wildcards for sharding) |
| `--port` / `-P` | `WEB_PORT` | 8080 | HTTP server port |
| `--hls-aac-codec` | `HLS_AAC_CODEC` | `libfdk_aac` | Audio codec |
| `--player` | `PLAYER` | false | Enable web player at `/player/` |
| `--disable-video-transcoding` | `DISABLE_VIDEO_TRANSCODING` | false | Skip video re-encoding |
| `--passthrough-video-codecs` | `PASSTHROUGH_VIDEO_CODECS` | empty | Source video codecs handed to players as they are (`hevc`); empty passes none. See docs/session-transcoding.md, Video Route |
| `--passthrough-video-codecs-file` | `PASSTHROUGH_VIDEO_CODECS_FILE` | empty | File with the same list, re-read on change (ConfigMap switch without restart); overrides the flag |
| `--unknown-session-delay` | `UNKNOWN_SESSION_DELAY` | 2s | Hold of the 404 of a playlist/segment request for a session this pod does not have; 0 answers at once. Keep < 2.5s (thp TTFB bucket) and < 10s (hls.js TTFB). thp records the held 404s in the transcoder's TTFB at ~2 s (status 400): TranscoderTTFBSlow and the TTFB panels must select `status!="400"`, shipped with this |
| `--debug` | `DEBUG` | false | Enable debug logging |
| `--clean-on-startup` | `CLEAN_ON_STARTUP` | false | Clean output directory on startup |
