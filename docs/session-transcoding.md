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

Errors: content-level rejections (`ErrResolutionNotSupported`,
`ErrTranscodingDisabled` — the source can never be transcoded by this
deployment) return **415** with the reason as plain-text body, so upstream
UIs can show a specific message (web-ui maps the body to a localized error
via `ClassifyError`). Transient internal failures return a generic **500**
to avoid leaking internals.

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
5. Serve the file of the session's current run (`serveSegment`): `Cache-Control: no-cache`, a strong `ETag`, no `Last-Modified`

#### Caching: validators name the run, not a date

A session URL outlives the run behind it. `/session/{id}/v0-720-0.ts` means "segment 0 of whatever run the session points at now", and a seek changes the bytes without changing the name. That is why segments and playlists go out with `Cache-Control: no-cache`: the browser must revalidate every time.

What a revalidation returns depends on the validator:

- **Segments (`.ts`, `.vtt`)** carry `ETag: "<generation>-<size hex>"`. The generation is a random name given to each FFmpeg process of a run (`TranscodeRun.generation`, renewed in `watchProcessLocked`).
  - Within one process a segment file is written once and only appended to, so its size identifies its state. A copy taken while FFmpeg was still writing therefore does not validate the finished file.
  - Any other run, or another process of the same run (an auto-restart rewrites the files from 0), has a different generation. A copy from it never validates, even when the sizes match. Audio and subtitle segments of two runs can match to the byte.
  - `If-None-Match` within one process returns 304, as cheap as before.
- **No `Last-Modified`.** Until 2026-09-26 `http.ServeFile` validated segments by mtime. A seek into a run left by an earlier viewer (still within its grace period) served files older than the browser's copy from the run it had just left. `If-Modified-Since` then returned 304, and the player played the pre-seek bytes on the new run's clock: 0:00–0:16 shown as 19:30–19:46. `ServeContent` gets a zero modtime, so it ignores `If-Modified-Since` and a dated `If-Range`. A copy with only a date gets the full segment.
- **Playlists** are rebuilt on every request and go out with no validator, so they are always 200.

Re-downloads this costs compared with date validation: none within one run process. The extra downloads fall in three cases, and each is a case where the bytes did or could change: after a seek, including a seek back to a run the browser had cached earlier (the cache holds one copy per URL, the last run's); after an auto-restart of the run; and a segment first fetched while it was still being written.

#### Auto-Restart Budget

Steps 2 (and the playlist handler's `EnsureRunning`) count consecutive
restart attempts per session. After `maxConsecutiveRestarts` (5) attempts
with no successfully produced segment in between, both entry points return
`ErrRestartLimit` and the handlers answer `503` immediately. Without the cap,
a source that keeps killing FFmpeg loops forever: the player re-requests the
segment, `Touch()` keeps the session alive past the reaper, and every attempt
spawns a doomed FFmpeg (observed live for 34h straight on one session). The
budget resets on a successful segment read (`WaitForSegment`) and on explicit
`Start`/`Seek` — user action may always try again.

### Playlist Request (GET /session/{id}/{stream}.m3u8)

1. Read FFmpeg's `.ffmpeg` file from the run's output directory
2. Clean: remove `#EXT-X-ALLOW-CACHE:YES` and `#EXT-X-ENDLIST`
3. Inject `#EXT-X-PLAYLIST-TYPE:EVENT` if missing
4. Inject `#EXT-X-START:TIME-OFFSET=0` so iOS Safari starts at the beginning instead of the live edge
5. Inject `#EXT-X-SESSION-OFFSET:<seek_seconds>` — movie-time of segment 0 in this variant. Read by downstream proxies (THP grace-window math) and ignored by players per RFC 8216 §3.1
6. Return as `application/vnd.apple.mpegurl`

The same `#EXT-X-SESSION-OFFSET` tag is also injected into the master `index.m3u8` in `services/web.go` `sessionPlaylistHandler`.

#### Subtitle Playlist Fallback

Subtitle streams (playlists matching `s{N}.m3u8`) use a shorter timeout (5s vs 5min). If FFmpeg cannot produce subtitle segments within that time (common with forced tracks that have little data), the handler returns a valid empty HLS live playlist:

```
#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:4
```

Without `#EXT-X-ENDLIST` — the player keeps polling, so if segments appear later they get picked up. This prevents subtitle issues from blocking video playback entirely.

Detection: `isSubtitlePlaylist(name)` checks the `s{digits}.m3u8` pattern. Every request does a quick check (`PlaylistForStream`) and then waits up to 5s while the run is going. Every wait is counted in `transcoder_playlist_waits_total{kind="subtitle"}`.

This fallback only protects the video from a subtitle track that is slow. A subtitle track FFmpeg cannot convert does not produce "no data": it makes FFmpeg refuse to open the outputs, and the whole run dies with the video and audio in it. That means bitmap codecs (PGS, `dvd_subtitle`, `dvb_subtitle`, `xsub`: "Subtitle encoding currently only possible from text to text or bitmap to bitmap") and streams without a text decoder ("Decoding requested, but no decoder found"). So `GetFFmpegParams` maps a subtitle stream only when its codec is in `textSubtitleCodecs` (`services/hls.go`). This is an allowlist of text codecs the production FFmpeg build decodes.

- **Numbering is not changed by this.** `hdmv_pgs_subtitle` streams are dropped from the HLS subtitle group entirely. Every other subtitle stream, mapped or not, keeps its `s{N}` slot in the master playlist. web-ui counts slots the same way (`embeddedSubtitleVisible` in `handlers/action/helper.go`) and uses `N` as the hls.js track index, so removing an entry would shift every later track.
- **An unmapped track gets the empty playlist above at once.** There is no 5s wait and no wait metric: hls.js re-polls a segment-less live playlist every few seconds, and one viewer with such a track selected would otherwise tip `TranscoderSessionsStuck`.
- **Streams are mapped by their input index (`-map 0:{index}`), never by type-relative position (`0:s:{n}`).** `n` counts only the streams `NewHLS` kept, while FFmpeg's `0:s:n` counts all of them. Until 2026-09 that mismatch mapped a PGS track that sat before a text track into the webvtt encoder.

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

### Pacing

A run is kept from getting too far ahead of its viewers (`services/pacing.go`).

**Why.** Measured on 2026-09-25 over 16 h of runs released for inactivity: copy runs produced a median 11 min of media, and at least 93% of it could never have been watched. For audio-only runs it was 99%, with p90 5.8 h ahead of the viewer. All of that was torrent data read from a seeder and written to the node's disk, and for re-encodes it was also CPU taken from the other runs on the pod.

**How.**
- The run tracks demand: the furthest segment number any session has requested from it. `sessionSegmentHandler` records it through `Session.noteDemand`.
- A per-process loop polls every second whether the primary stream's segment `demand + lead` exists.
  - If it does, FFmpeg's process group is frozen with `SIGSTOP`.
  - It is continued with `SIGCONT` once segment `demand + lead − 60 s` no longer exists, i.e. a viewer has come within 4 minutes. The 60 s gap is hysteresis, so the run advances in bursts of about a minute.
- `PACE_LEAD` (flag `--pace-lead`) sets the lead. The default is 5 min; `0` disables pacing.
- Before the first request demand counts as 0, so the first lead of every run is produced at full speed and start-up is untouched.

**Details.**
- A frozen process's source connection just idles. Neither torrent-http-proxy nor the seeder sets read/write timeouts. `-reconnect 1 -reconnect_on_network_error 1` on the input resumes a dropped connection with a Range request.
- Stopping a frozen run needs nothing special. Stop cancels the run's context, and `exec.CommandContext` kills the process with `SIGKILL`, which a stopped process does not hold back.
- A new process of the run (a restart) resets demand, because segment numbers start over.
- Speed (`transcoder_run_speed`, the `speed` field of `run: ffmpeg ended`) is media time over the time FFmpeg was allowed to run. FFmpeg's own `speed=` counts frozen time too.
- Metrics:
  - `transcoder_runs_paused`: processes frozen right now;
  - `transcoder_run_pause_seconds_total{mode}`: total time processes spent frozen.

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
ffmpeg -ss {time} -i {url} ... -c:v h264 -preset veryfast ...
```

- `-ss` before `-i` here too (`injectSeekParams` places it there in both modes). FFmpeg seeks the input to the keyframe before `{time}` using the container index, then decodes and discards frames up to `{time}` (accurate seek is the default).
- Perfect A/V sync: both streams are decoded and start from the exact position.
- On any seek (`{time}` > 0), `-xerror` is removed. AVI and other containers report non-fatal errors after a seek, and `-xerror` would turn them into a failed run.
- From the start (`{time}` = 0) `-xerror` stays. Without it, a failed read of the source ends FFmpeg like the end of the file: exit 0, a completed run, and it is never restarted.
- Per-source fallbacks (`ParamOptions`, remembered by the RunManager per source). When a run dies on a failure a known option cures, that source's later runs get the option:
  - timestamps (`Non-monotonic DTS` / `Invalid DTS` under `-xerror`) → `Lenient`, which drops `-xerror`;
  - `Scalable configurations are not allowed in ADTS` → `EncodeAudio`, which re-encodes AAC the probe would copy.

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

## Metrics

Prometheus metrics (`services/metrics.go`) are served by common-services on
the prom port (8083, `--use-prom`, `httpprom` in the chart). Namespace
`transcoder`; label sets are closed — no session ids, hashes or paths.

| Metric | Meaning |
|---|---|
| `sessions_total`, `sessions_active` | Sessions created / held by the SessionManager |
| `runs_total{outcome}` | FFmpeg processes that ended: `finished` (source done), `released_idle` (run reaper), `killed` (shutdown, explicit Stop), `failed` (died on its own) |
| `runs_active` | FFmpeg processes running |
| `ffmpeg_exits_total{reason}` | How the process ended: `ok`, `error` (non-zero), `signal` |
| `playlist_waits_total{outcome,kind}` | `WaitForPlaylist` results: `ok`/`timeout`/`not_running`/`canceled` × `variant`/`subtitle` (subtitle timeouts are expected, see `emptySubtitlePlaylist`) |
| `playlist_wait_seconds{kind}` | Time until a playlist appeared, successful waits only |
| `auto_restarts_total` | Auto-restart attempts charged to a session's budget |
| `restart_limit_reached_total` | Sessions that hit `maxConsecutiveRestarts` (once per session) |
| `source_open_seconds{outcome}` | Time to probe a source (its first read); cached probes excluded |

A run's outcome is recorded once, when the process is reaped
(`TranscodeRun.reapProcess`): whoever stops it leaves the reason in
`stopReason` first, so a process that dies on its own (`failed`) is told
apart from one we ended. `failed` + `signal` is an OOM kill or the node;
`failed` + `error` is FFmpeg giving up on the source.
