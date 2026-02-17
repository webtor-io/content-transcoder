# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

HTTP stream to HLS (HTTP Live Streaming) transcoder. Accepts a source URL, transcodes it using FFmpeg, and serves the resulting HLS segments via an HTTP server. Part of the [webtor.io](https://webtor.io) ecosystem.

## Build & Run

```bash
# Build
go build -mod=vendor -o server .

# Run locally (requires FFmpeg 3+ installed)
./server --input='https://example.com/video.mkv' --player=true
# Then open http://localhost:8080/player/

# Docker build (multi-stage: golang → jrottenberg/ffmpeg:8-alpine)
docker build -t content-transcoder .
```

There are no tests in this project. The `.env` file contains local development overrides.

## Architecture

### Service Initialization (configure.go)

All services are wired in `configure.go:run()`. The CLI framework is `urfave/cli` with flags registered per-service via `Register*Flags()` functions. Services implement `cs.Servable` from `github.com/webtor-io/common-services`.

### HTTP Middleware Chain (services/web.go)

Requests flow through a middleware chain built in `Web.buildHandler()`:

```
setContextHandler  → Parses source URL (from X-Source-Url header or ?source_url= param),
                     creates SHA1-based output directory path, stores in request context
  → doneHandler    → Checks if transcoding is complete (for ?done queries)
  → touchHandler   → Updates access timestamp in TouchMap (grace period tracking)
  → allowCORSHandler
  → waitHandler    → Polls every 500ms until sub-playlist (.m3u8) is ready
  → enrichPlaylistHandler → Injects query params into HLS segment URLs
  → transcodeHandler → Triggers async FFmpeg transcode on /index.m3u8 requests
  → fileHandler    → Serves files from the output directory
```

### Key Services (services/)

- **ContentProbe** (`content_prober.go`) — Probes media metadata via gRPC (remote content-prober service) or local ffprobe. Caches results in `index.json` within the output dir.
- **HLSBuilder/HLS** (`hls.go`) — Generates FFmpeg arguments for multi-rendition HLS encoding (240p–1080p). Builds master playlist with video, audio, and subtitle stream groups.
- **TranscodePool** (`transcode_pool.go`) — Uses `lazymap` to ensure single concurrent transcode per output path. Tracks in-progress and completed jobs.
- **TouchMap** (`touch_map.go`) — Tracks last access time per output directory. Used to implement inactivity-based cleanup (default grace: 600s).
- **Transcoder** (`transcoder.go`) — Wraps FFmpeg execution. Logs to `ffmpeg.out`/`ffmpeg.err` files in the output dir.

### Request Context Keys

- `OutputDirContext` — Resolved output directory path (based on SHA1 hash of source URL path)
- `SourceURLContext` — Original source media URL

### Output Directory Naming

Source URL path → SHA1 hash → subdirectory under `--output` flag. Supports wildcard output paths (`/data*`) for distribution across multiple mount points via consistent hashing (`common.go:DistributeByHash`).

## Key Configuration (env vars / CLI flags)

| Flag | Env Var | Default | Purpose |
|------|---------|---------|---------|
| `--input` / `-i` | `INPUT`, `SOURCE_URL`, `URL` | — | Source media URL |
| `--output` / `-o` | `OUTPUT` | `out` | Output directory |
| `--port` / `-P` | `WEB_PORT` | 8080 | HTTP server port |
| `--access-grace` | `GRACE` | 600 | Inactivity timeout (seconds) |
| `--preset` | `PRESET` | `ultrafast` | FFmpeg encoding preset |
| `--hls-aac-codec` | `HLS_AAC_CODEC` | `libfdk_aac` | Audio codec |
| `--player` | `PLAYER` | false | Enable web player at `/player/` |
| `--disable-video-transcoding` | `DISABLE_VIDEO_TRANSCODING` | false | Skip video re-encoding |