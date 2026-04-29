# Session-Based Transcoding Architecture

## Overview

The transcoder uses a session-based model where each viewer creates a session via API. Sessions manage HLS playback with seek support. Multiple sessions watching the same content at the same position share a single FFmpeg process via the RunManager.

## Session Lifecycle

### Create (POST /session?source_url=...)

1. Probe media via ffprobe (cached in `index.json`)
2. Build HLS params from probe result
3. Create session with unique ID
4. Write master playlist (`index.m3u8`) to session directory
5. Acquire a shared `TranscodeRun` at position 0 via `RunManager`
6. Return `{ id, duration }` to player

### Seek (POST /session/{id}/seek?t=...)

1. Quantize seek time to 30s boundary (`quantizeSeekTime`)
2. Release current `TranscodeRun` (only after new one is acquired)
3. Acquire new `TranscodeRun` at quantized position
4. Player reloads HLS from new position

### Segment Request (GET /session/{id}/{segment}.ts)

1. Update `lastAccess` and `.touch` file
2. If FFmpeg is not running → re-acquire run at current `seekTime`
3. Wait for segment file to appear on disk (200ms polling, 5min timeout)
4. Return early if FFmpeg exits without producing the segment
5. Serve file via `http.ServeFile`

### Playlist Request (GET /session/{id}/{stream}.m3u8)

1. Read FFmpeg's `.ffmpeg` file from the run's output directory
2. Clean: remove `#EXT-X-ALLOW-CACHE:YES` and `#EXT-X-ENDLIST`
3. Inject `#EXT-X-PLAYLIST-TYPE:EVENT` if missing
4. Inject `#EXT-X-START:TIME-OFFSET=0` so iOS Safari starts at the beginning instead of the live edge
5. Inject `#EXT-X-SESSION-OFFSET:<seek_seconds>` — movie-time of segment 0 in this variant. Read by downstream proxies (THP grace-window math) and ignored by players per RFC 8216 §3.1
6. Return as `application/vnd.apple.mpegurl`

The same `#EXT-X-SESSION-OFFSET` tag is also injected into the master `index.m3u8` in `services/web.go` `sessionPlaylistHandler`.

#### Subtitle Playlist Fallback

Subtitle streams (playlists matching `s{N}.m3u8`) use a shorter timeout (5s vs 5min). If FFmpeg cannot produce subtitle segments within that time (common with forced/bitmap subtitle tracks that have no data), the handler returns a valid empty HLS live playlist:

```
#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:4
```

Without `#EXT-X-ENDLIST` — the player keeps polling, so if segments appear later they get picked up. This prevents subtitle issues from blocking video playback entirely.

Detection: `isSubtitlePlaylist(name)` checks the `s{digits}.m3u8` pattern. The first request does a quick check (`PlaylistForStream`) then waits up to 5s. Subsequent requests return the empty playlist immediately if the file still doesn't exist.

### Inactivity

- **60s idle** → Session releases its run (FFmpeg may continue for other sessions)
- **10min idle** → Session removed entirely
- **Run with 0 refs** → 30s grace period, then FFmpeg stopped and directory cleaned

### Close (DELETE /session/{id})

1. Release the run
2. Remove session directory (master playlist only)
3. Remove from SessionManager

## Shared Runs (TranscodeRun)

A `TranscodeRun` is one FFmpeg process writing segments to `{hashDir}/runs/seek-{time}/`. It is reference-counted — multiple sessions can share it.

### Run Identity

Runs are keyed by `(hashDir, seekTime)`. Two sessions with the same source URL and same quantized seek time share the same run.

### Seek Quantization

Seek times are rounded down to 30-second boundaries:

```
seekTime=0     → 0       (no quantization for start)
seekTime=500   → 480     (floor(500/30) * 30)
seekTime=510   → 510     (exact boundary)
seekTime=1000  → 990
```

This ensures viewers seeking to nearby positions share FFmpeg processes and segments.

### Reference Counting

```
Viewer A creates session     → Acquire(hash, 0.0) → Run#1 created, refCount=1
Viewer B creates session     → Acquire(hash, 0.0) → Run#1 reused, refCount=2
Viewer A seeks to 500        → Release(Run#1, refCount=1), Acquire(hash, 480.0) → Run#2
Viewer B closes              → Release(Run#1, refCount=0) → grace 30s → cleanup
```

### Grace Period

When refCount drops to 0, the run enters a 30-second grace period before cleanup. This allows a viewer who seeks away and then seeks back to reuse the same run without restarting FFmpeg.

## FFmpeg Seek Strategy

### Copy Mode (h264 source → `-c:v copy`)

```
ffmpeg -ss {time} -noaccurate_seek -i {url} ... -c:v copy ...
```

- `-ss` before `-i`: fast input-level seek (keyframe-based)
- `-noaccurate_seek`: disables frame trimming between keyframe and target. Both video (copy) and audio (re-encode) start from the **same keyframe** → perfect A/V sync
- Segments numbered from 0, PTS from 0

### Re-encode Mode (mpeg4, vp9, etc. → `-c:v h264`)

```
ffmpeg -i {url} -ss {time} ... -c:v h264 -preset veryfast ...
```

- `-ss` after `-i`: output-level seek. FFmpeg decodes from the beginning and discards frames until the target position
- Required for containers like AVI over HTTP that don't support input-level seeking (no byte-range support, no index)
- Slower but always works
- Perfect A/V sync (both streams decoded and started from exact position)

## Player (player/index.html)

The HLS.js-based player manages sessions:

1. **Init**: `POST /session` → get session ID and duration
2. **Load**: HLS.js loads `/session/{id}/index.m3u8`
3. **Seek**: Custom seekbar → `POST /session/{id}/seek?t=` → reload HLS
4. **UI**: Overlay with spinner during seek, play/pause, volume, keyboard shortcuts
5. **Cleanup**: `navigator.sendBeacon` on page unload

The player tracks `seekOffset` — the quantized seek position. Displayed time = `seekOffset + video.currentTime`.

## Directory Structure

```
{output}/
  {sha1_hash}/                     # Per-content (SHA1 of source URL path)
    {sha1_hash}.touch              # Access marker for external cleanup
    index.json                     # Cached probe result
    sessions/
      {sessionID}/
        index.m3u8                 # Master playlist (per-session, static)
    runs/
      seek-0.000/                  # Shared run: transcoding from 0s
        v0-720-0.ts, v0-720-1.ts  # Video segments
        a0-0.ts, a0-1.ts          # Audio segments
        v0-720.m3u8.ffmpeg         # FFmpeg's raw playlist
        a0.m3u8.ffmpeg
        ffmpeg.out, ffmpeg.err     # FFmpeg logs
      seek-480.000/                # Shared run: transcoding from 480s
        ...
```

## Key Constants

| Constant | Value | Location | Purpose |
|----------|-------|----------|---------|
| `seekQuantum` | 30s | session.go | Seek time quantization step |
| `sessionSegDuration` | 4s | session.go | HLS segment duration |
| `sessionInactivityRelease` | 60s | session_manager.go | Release run after inactivity |
| `sessionInactivityExpiry` | 10min | session_manager.go | Remove session after inactivity |
| `runGracePeriod` | 30s | run_manager.go | Keep idle run alive for reuse |
| `runGracefulStopTimeout` | 2s | transcode_run.go | SIGTERM → SIGKILL timeout |
