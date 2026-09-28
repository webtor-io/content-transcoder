# Session-Based Transcoding Architecture

## Overview

The transcoder uses a session-based model where each viewer creates a session via API. Sessions manage HLS playback with seek support. Multiple sessions watching the same content at the same position share a single FFmpeg process via the RunManager.

## Session Lifecycle

### Create (POST /session?source_url=...)

1. Probe media via ffprobe (cached in `index.json`)
2. Build HLS params from probe result
3. Decide the video route (see [Video Route](#video-route-hevc-passthrough)), then the audio outputs (see [Audio](#audio-multichannel-aac-and-dolby))
4. Create session with unique ID
5. Write master playlist (`index.m3u8`) to session directory
6. Acquire a shared `TranscodeRun` at position 0 via `RunManager`
7. Return `{ id, duration, video_route, route_reason }` to player

Errors: content-level rejections (`ErrResolutionNotSupported`,
`ErrTranscodingDisabled` — the source can never be transcoded by this
deployment) return **415** with the reason as plain-text body, so upstream
UIs can show a specific message (web-ui maps the body to a localized error
via `ClassifyError`). Transient internal failures return a generic **500**
to avoid leaking internals. A refusal the route causes — the old route
will not encode the video (over 1080p, or `DISABLE_VIDEO_TRANSCODING`) —
names the route reason in `X-Video-Route-Reason`; the body stays what it
was. A source with nothing playable (`no video or audio stream`) is refused
whatever the route and has no such header. When the transcoder's own look
at an HEVC source failed and the old route refuses it (over 1080p), the
answer is **503** `source check failed` with `Retry-After: 5` and the
header instead of the 415: a check that did not answer is not a source
that cannot play.

### Video Route (HEVC passthrough)

A session either takes the **old route** — h264 copied into TS, other video
re-encoded to h264 up to 1080p, over 1080p refused with 415 — or
**passthrough**: the source's HEVC handed to the player as it is (fMP4,
`-c:v copy -bsf:v hevc_mp4toannexb -tag:v hvc1`). The transcoder decides;
the client only declares what it decodes.

- **Declaration.** `POST /session?...&decode=<tokens>`, comma-separated:
  `hevc8`, `hevc10`, `hevc8-2160`, `hevc10-2160`, `hevc-high`, `hdr-pq`;
  the audio tokens `aac51`, `ac3`, `ec3` (see [Audio](#audio-multichannel-aac-and-dolby));
  or `unknown` (the client's check had not answered). Exact allowlist match;
  unknown tokens are ignored, an empty, garbage or over-512-byte value is no
  declaration. A higher token covers the lower (`hevc10` covers Main, a
  `-2160` token its depth at 1080). The log's `decode` lists them in a fixed
  order: `unknown`, the video tokens, the audio tokens.
  - **Audio tokens and the video route.** The video checks read the video
    tokens only. A declaration of audio tokens alone names no video: never
    `no_declaration`; past the checks every declaration gets
    (`passthrough_off`, `not_hevc`, `declaration_pending`) it is
    `no_hevc_declared`, decided before the size checks and the source probe
    — with no HEVC token the probe could only answer `needs_main` /
    `needs_main10`, so it is not spent. A declaration with any video token
    (`hdr-pq` alone included) goes on through the checks as before.
    `unknown` next to audio tokens only is still pending for the video
    (`declaration_pending`): an audio answer is not an answer about HEVC.
    Adding audio tokens to a declaration with video tokens changes no video
    decision (`TestVideoRouteFor_AudioTokensDoNotMoveTheVideo`).
- **Capability.** `--passthrough-video-codecs` / `PASSTHROUGH_VIDEO_CODECS`
  lists the source codecs passed through (`hevc`); empty — the default —
  passes none. `--passthrough-video-codecs-file` /
  `PASSTHROUGH_VIDEO_CODECS_FILE` overrides it and is re-read on every new
  session when it changed (mtime, size or inode: a ConfigMap mounted as a
  directory), so passthrough is switched without a restart; sessions already
  open keep their route. An unreadable file keeps the last value. Only codecs
  this build can write count (`passthroughBuildCodecs`); the line
  `HEVC passthrough: on|off` at start (`source` the flag), at the file's
  first read (`source` the file, even when it says the same) and on every
  change says what is in effect and what was ignored.
- **Decision** (`videoRouteFor`, `services/route.go`), first match wins; the
  checks before the source probe need nothing but content-prober's answer:
  `no_declaration`, `passthrough_off`, `not_hevc`, `declaration_pending`,
  `no_hevc_declared` (audio tokens only), `too_large` (over 3840×2160),
  `needs_2160` (over 1080 — taller or wider — without a 2160 token); then the source probe: `probe_failed`, `dv5`,
  `dv7`, `dv_base`, `dv_unknown` (RPU NAL 62/63 without a record),
  `pix_fmt`, `interlaced`, `no_hvcc` (not an hvcC, or its arrays lack a
  base-layer VPS, SPS or PPS, or hold a NAL type `hevc_mp4toannexb`
  refuses), `profile` (not Main/Main10),
  `too_large` (level over 5.1), `needs_main10` / `needs_2160` /
  `needs_main` (depth and level against the tokens; level over 4.1 needs a
  2160 token), `needs_high_tier`, `needs_pq`, `hlg_later`; otherwise `ok`.
  Every reason except `ok` is the old route, unchanged.
- **Source probe** (`services/source_probe.go`): one ffprobe of the video
  stream over the source URL (`-probesize 5000000`, stream, extradata as
  hvcC, Dolby Vision record, the first 2 packets with their data, and the
  keyframe among them decoded — `-show_frames -skip_frame nokey`), 5 s,
  one retry. Successes are cached in `{hashDir}/source-video-{index}.json`
  (layout version 2); failures are not. No decoded frame is a failed probe.
  - **Transfer.** ffprobe's stream-level `color_transfer` is the
    container's whenever it has colour information: an MKV whose Colour
    element has a matrix and no transfer reports none over a PQ bitstream.
    The decoded frame's is the bitstream's (VUI, or an
    alternative-transfer SEI). The route reads the stronger of the two
    (`sourceHEVCFacts.transfer`): HLG over PQ over anything else — PQ in
    either needs `hdr-pq` and is labelled `VIDEO-RANGE=PQ`, HLG in either
    is `hlg_later`. The decode costs 8–26 ms on the e2e sources and 0.6 s
    on a 7.3 MB 4K 10-bit keyframe (2 CPUs), once per source and node.
  - **Profile, tier, level** are read as the output will carry them
    (`outputHVCC`): `hevc_mp4toannexb` turns the record's arrays into
    Annex B and movenc writes a new hvcC from them
    (`ff_isom_write_hvcc`): from its defaults it merges the
    profile_tier_level of every base-layer VPS and SPS (the higher tier and
    its level, the higher profile, flags all sets have) and never reads the
    source record's head. A head that disagrees with the SPS therefore does
    not decide: measured on 8.1.2, heads patched to L153 over an SPS at L30
    and to L30 over H153 came out `hvc1.1.6.L30.90` and `hvc1.1.6.H153.90`,
    as `outputHVCC` predicts. movenc drops the parameter sets from the
    samples of an hvc1 track, so a record without all three is `no_hvcc`.
- **The 415 over 1080p** stands for "the video would have to be encoded":
  the passthrough route is not subject to it (nor to
  `DISABLE_VIDEO_TRANSCODING`); the old route keeps it as it was.
- **Old route unchanged.** With the capability empty, or without a usable
  declaration, arguments, playlists, refusals and run layout are byte for
  byte those of 1b25e28 (`golden_old_route_test.go` records them there,
  `golden_route_test.go` replays them), apart from the deliberate seek fixes
  listed in `golden_old_route_test.go` (see
  [FFmpeg Seek Strategy](#ffmpeg-seek-strategy)). Audio tokens that change
  no audio output of the source change no argument, playlist, segment or
  run either (only the route reason, which is a declaration's:
  `no_hevc_declared` for audio tokens alone, see above);
  the declared cases have their own record (`testdata/golden_audio.json`,
  `golden_audio_test.go`).
- **Output** (`services/passthrough_output.go`, `passthrough_web.go`), see
  [Passthrough output](#passthrough-output).

#### Passthrough output

- **Command** (`buildPassthroughParams`). The input side is the old
  route's. The video and every audio track go through FFmpeg's hls muxer as
  fMP4 (fMP4 video next to TS audio was never tried in a player, so they are
  not mixed): `-map 0:<i> <codec> -f hls -hls_time 4 -hls_list_size 0
  -hls_playlist_type event -hls_segment_type fmp4 -hls_flags temp_file
  -hls_fmp4_init_filename <prefix>-init-<gen>.mp4 -hls_segment_filename
  <run>/<prefix>-%d.m4s <run>/<prefix>.m3u8.ffmpeg`. Video
  `-c:v copy -bsf:v hevc_mp4toannexb -tag:v hvc1` (parameter sets out of the
  samples, hvc1 for Apple), audio copied or encoded by the one audio
  decision (see [Audio](#audio-multichannel-aac-and-dolby)): as on the old
  route without audio tokens, E-AC-3 / AC-3 copied with `ec3` / `ac3`.
  Subtitles exactly as on the old route (segment muxer, webvtt). The video
  is cut at keyframes only.
- **Init named after the process.** `<gen>` is the run process's generation,
  made before its arguments (`startLocked`). A restart reuses the run
  directory, and hlsenc opens the init when it starts and fills it only at
  its first cut: under a fixed name the new process would empty the init the
  previous playlist still names. hlsenc writes the init whole and closes it
  at the first cut before that cut's segment and playlist.
- **Seek** — see [Passthrough Mode](#passthrough-mode-hevc--fmp4).
- **Master** (`writePassthroughMaster`). Not written at POST /session: the
  first request for `index.m3u8` restarts a stopped run (`EnsureRunning`),
  waits (up to 5 min) until the video init of the run's current process is
  complete — a process that ends before its init is restarted once more
  and waited for, and if that one ends too the answer is 504, logged as
  `the run ended before its init` — and writes the master from it: `CODECS` from the init's hvcC
  (`hevcCodecString`, ISO/IEC 14496-15 Annex E — `hevc_mp4toannexb`
  rebuilds the record, so not the source's), then every codec the audio
  renditions have (`mp4a.40.2` without audio tokens; `ec-3`, `ac-3` for
  copied Dolby), `RESOLUTION` from content-prober, `VIDEO-RANGE=PQ` for a source
  whose transfer (above) is `smpte2084` (else `SDR`), `BANDWIDTH` = the
  larger of the source's
  average bit rate and the first video segment's rate plus the audio: 192 kb/s,
  or with a declaration that changes the audio the largest audio output's
  rate. A profile, tier or level that differs from what the route was
  decided on (`outputHVCC` of the source) is counted
  (`passthrough_codecs_mismatch_total{field}`);
  an init no CODECS can be read from is counted as `unbuildable` and the
  master is answered 500 — a guessed CODECS fails in the player at once.
  Written once, atomically; later reads serve it with the session's
  `#EXT-X-SESSION-OFFSET` like the old master. Its audio part is what the
  run makes: the run's options (after a fallback, the AAC it encodes from
  the next start) and the decisions of the session that started the run
  (`runHLS`, see the known cost under Audio, Run variant); with a
  declaration that changes the audio a read after the run learned a
  fallback rewrites it (`refreshMaster`).
- **Media playlists** come from hlsenc as they are (VERSION 7, EVENT,
  `#EXT-X-MAP:URI="<prefix>-init-<gen>.mp4"`) and get the usual treatment
  (`PlaylistForStream`: ENDLIST only once the run completed,
  `#EXT-X-START`, `#EXT-X-SESSION-OFFSET`). The client's query is appended
  to every reference, the MAP URI included, by a token pattern with
  boundaries (`passthroughRefPattern`): the old, unanchored pattern finds
  nothing in an init name whose generation ends in, say, `e90` (the init
  would go out without the token) and a partial `a12.mp4` in one ending in
  `a12`. Old-route playlists keep the old pattern, byte for byte.
- **Serving.** Only a passthrough session answers `.m4s` (its own streams'
  segments, through the segment handler: demand, restart, `ETag`) and
  `<prefix>-init-<16 hex>.mp4`; anything else ending in `.mp4` or `.m4s` is
  404, as before. Both go out as `video/mp4`, set explicitly (Go has no
  `.m4s`, the image has no `/etc/mime.types`). An init has its own branch
  before any segment logic (an all-digit generation would parse as a segment
  number): no demand, no restart for a segment; `EnsureRunning` like a
  playlist. It is served only complete: the running process's once its
  playlist names it (waiting up to 10 s, then 404), an earlier process's
  if it has bytes (else 404 at once). Its `ETag` is the generation in its
  name and its size.
- **Cleanup.** Init files of earlier processes stay in the run directory
  (a few KB each, at most one per restart) and go with it when the run is
  cleaned up.

### Audio (multichannel AAC and Dolby)

Without audio tokens every audio track is what it always was: AAC with up
to 2 channels copied, everything else encoded to AAC stereo
(`<aacCodec> -ac 2`). The tokens change that per track; one function,
`audioOutputFor` (`services/audio.go`), decides, and the run's arguments
(`codecParams`), the re-encode seek cuts (`reencodeSeekCuts`, through
`codecParams`), the masters and the run variant all read it.

| Source track | Declared | fMP4 (passthrough) | Output |
|---|---|---|---|
| AAC ≤ 2 ch | anything | either | copy (as always) |
| AAC 3–6 ch in a channel configuration (layout `3.0`, `4.0`, `5.0`, `5.1`) | `aac51` | either | copy (its ADTS channel configuration, 6 for 5.1, in TS) |
| E-AC-3 > 2 ch | `ec3` | yes | copy (`ec-3`, `dec3` with the JOC extension when the stream has it) |
| AC-3 > 2 ch | `ac3` | yes | copy (`ac-3`, `dac3`) |
| any other > 2 ch (AAC 7.1, AAC with a PCE, E-AC-3/AC-3 without their token or on TS, DTS, TrueHD, FLAC …) | `aac51` | either | `<aacCodec> -ac 6 -b:a 384k` |
| everything else (stereo non-AAC included) | — | — | `<aacCodec> -ac 2`, as always |

- **Dolby only on fMP4.** hls.js 1.6.14 (web-ui's full build) throws
  "Unsupported EC-3 in M2TS" (`tsdemuxer.ts`) and plays AC-3 in TS only in
  builds with it; the old route's audio is always TS and passthrough's always
  fMP4, never mixed. **AC-3 needs `ac3`**: an E-AC-3 decoder decodes AC-3,
  but the token is the browser's MediaSource answer for the codec string, and
  the copy carries `ac-3`. **Stereo Dolby stays AAC stereo**: a copy gains
  nothing audible there and leans on the browser's answer.
- **AAC only in a channel configuration.** An AAC track whose channels a
  program config element (PCE, channel configuration 0) declares is not
  played by Chrome 154 with hls.js 1.6.14 in TS or fMP4 (MediaError 4,
  `bufferAppendError`), and FFmpeg copies it without a word (exit 0). The
  copy is kept for the layouts of configurations 3–6 as FFmpeg names them
  (`aacConfigLayouts`: `3.0`, `4.0`, `5.0`, `5.1` — `ff_aac_ch_layout`; the
  decoder names a stream's layout from its elements, so that is what
  ffprobe, and content-prober, reports). Anything else is a PCE and is
  encoded to 5.1: FFmpeg's own encoder writes one for 5.1(side) (ffprobe:
  no layout — 9 of 526 six-channel AAC tracks in 24 h of production
  probes), quad and 2.1 (`quad`, `2.1`). libfdk_aac maps 5.1(side) to
  configuration 6 (`5.1`). A PCE that declares exactly a configuration's
  elements would be named like it and still be copied: content-prober gives
  no extradata to tell; FFmpeg's encoder never writes one.
- **384 kb/s.** Without `-b:a` libfdk_aac takes `(96·SCE + 128·CPE) ·
  rate / 44` (`libfdk-aacenc.c`): 489 kb/s for 5.1 at 48 kHz. 384 kb/s is
  64 kb/s per full channel, the stereo default's share, and the rate web-ui
  asks `mediaCapabilities` about for `aac51`.
- **Layout.** `-ac 6` asks for 6 channels in no order; FFmpeg takes the
  encoder's first 6-channel layout (`ffmpeg_filter.c`, `set_channel_layout`),
  libfdk_aac's `5.1` (FL FR FC LFE BL BR), and libswresample remixes into it
  (`rematrix.c`). Measured on 8.1.2 through the transcoder's own options
  (`e2e/audio/layout.py`, a tone in one channel at a time, −9.1 dB in):
  - 5.1(side) (E-AC-3, AC-3, DTS, TrueHD): SL → BL and SR → BR at 1.0, every
    other channel to itself, no level change;
  - 7.1: SL mixed into BL and SR into BR at 1/√2; the BL row sums to 1.707
    and, the output being S16 (libfdk_aac's only format), the matrix is
    normalized by it: every channel −4.6 dB, SL/SR −7.6 dB (today's stereo
    downmix of 7.1 is normalized by 3.1, −9.9 dB).
- **Fallback.** `EncodeAudio` (after an ADTS failure) still encodes every
  track: 5.1 where `aac51` gives 5.1 or a multichannel copy, stereo elsewhere.
  - **A copy the muxer refuses.** movenc refuses E-AC-3 it cannot put in an
    ISOBMFF track (`handle_eac3`: several independent substreams, a frame
    that no longer parses once the track has samples) and the run dies,
    exit 183, `[aost#1:0/copy @ …] Error submitting a packet to the muxer:
    Invalid data found when processing input` (`copiedAudioMuxFailure`).
    With nothing learned every restart died the same way and after 5 the
    playlists answered 503. When the session copies audio only because the
    declaration made it (`copiesDeclaredAudio`), the run learns
    `EncodeAudio` **and** `Lenient`, remembered under the variant's
    `fallbackKey` (from a seek run too): the decoder then reads the bitstream
    the muxer refused, and on both sources measured (FFmpeg 8.1.2) it errs
    too (`corrupt decoded frame`, `Error submitting packet to decoder`),
    which `-xerror` makes fatal — an encode alone died at the same place.
    Every other session fails as before: without a declaration these
    sources' stereo encode dies on the decoder the same way (measured on
    079acfd: 503 after 5 restarts in 3.3–3.7 s), a pre-existing gap no
    fallback covers.
  - **The masters follow.** CODECS, CHANNELS and BANDWIDTH are made with
    the options of the run they describe: a passthrough master from the
    run's options when it is written, an old-route master from what the run
    manager remembers for the variant when the session opens; a read after
    the run learned a fallback rewrites either (`refreshMaster`, logged
    `session: master rewritten for the audio the run makes now`). Without a
    declaration that changes the audio neither depends on the options and
    both are written as before.
  - **Stale playlists.** A process that makes an audio output differently
    from the process before it starts without that output's old playlist
    (`dropChangedAudioPlaylistsLocked`): served until the new process wrote
    its own, it named the old process's `ec-3` init, which Chrome appended
    and failed on (`Unsupported audio format 0x65632d33 in stsd box`) while
    the master said AAC. Only with a declaration that changes the audio.
  - Measured (`e2e/audio/craft.py` sources, the review's construction:
    `av_eac3_multi.mkv`, every other frame independent substream 1;
    `av_eac3_corrupt.mkv`, frame 400 at 12.8 s broken): d06015f, declared
    `hevc8,aac51,ec3` — 503 after 3.3–3.4 s, master `ec-3`; now — one
    failed start, the run finishes (25 segments, ENDLIST, 1.8 s), master
    `mp4a.40.2` `CHANNELS="6"`, output AAC 5.1 in configuration 6; Chrome
    154 plays `av_eac3_corrupt.mkv` from the start and after a seek, and
    `av_eac3_multi.mkv` after a seek; from the start the latter stalls on
    a video gap (0.54–9.92 s): the dead first process's partial `v0-0`
    and playlist, what any early death and restart leaves (not measured
    on 079acfd, where such a restart needs another cause).
- **Master.** CODECS lists every audio codec the outputs have, once, in
  rendition order (the old route's is `avc1.42e00a,mp4a.40.2` as always: all
  its outputs are AAC). One audio group: RFC 8216 4.3.4.2 asks CODECS to name
  every format of every rendition, hls.js keeps a variant only if MediaSource
  takes every codec it lists (`level-controller.ts`), and it builds an
  alternate audio track's SourceBuffer from the track's own init
  (`passthrough-remuxer.ts` `getParsedTrackCodec`, `buffer-controller.ts`
  `pickMostCompleteCodecName`; the variant's codec is used only when it lists
  one audio codec), switching with `changeType`. When the declaration
  changes the audio, each audio rendition says `CHANNELS` (the output's
  count; `"2"` for an AAC copied without a count from content-prober — the
  rule copies it as up to 2 channels, and RFC 8216 4.3.4.1 makes the
  attribute REQUIRED on every rendition once one has it; E-AC-3 JOC would be
  `16/JOC` for Apple, but content-prober's answer has no profile to tell it
  by, so it is the channel count) and `BANDWIDTH`
  counts the largest audio output: an encode at its rate (384 kb/s, 192 kb/s
  stereo), a copy at the stream's `bit_rate`, else mkvmerge's `BPS` tag, else
  640 kb/s over 2 channels and 192 kb/s up to 2 — on the old route on top of
  the video's rate (which alone it was, and stays without a declaration), on
  passthrough instead of the 192 kb/s allowance. Codec rates, as the video's
  is: MPEG-TS adds its own (AAC 5.1 at 385 kb/s came to 455 kb/s of TS
  segments). In 24 h of production probes
  (2026-09-28) E-AC-3 5.1 had a `bit_rate` in 406 of 409 streams (median
  640, p90 768 kb/s), AC-3 5.1 in all but a few that have `BPS`; AAC 5.1
  had it in 2 of 107, `BPS` in 52, neither in 53 (p90 449 kb/s).
- **Run variant.** A declaration that changes an audio output gets runs of
  its own: `HLS.runVariant` joins `hevc` (passthrough) and the audio variant,
  `a` and one code per audio output (`c` copy, `2`/`6` the encode's
  channels): key `{hashDir}:a6c:seek:{t}`, directory `a6c-seek-{t}`,
  `hevc-accc6666-seek-{t}`. Without a change the part is empty and the key,
  directory and argv are the old ones, so old and new pods of a rollout keep
  sharing those runs. Declarations that give the same arguments share runs
  (`ac3` on a TS session changes nothing). The remembered real start and
  FFmpeg options are per key too (`fallbackKey`): a variant learns its own
  fallbacks, at the price of one failed start of its own.
  - **Known cost: fallbacks and declarations that share a run.** The
    variant is decided without fallbacks, so declarations whose arguments
    are equal without them share a run, its fallbacks and its directory —
    and after `EncodeAudio` their arguments would differ: a copied E-AC-3
    becomes AAC 5.1 with `aac51` and AAC stereo without. The run keeps the
    arguments of the session that started it: a session of `hevc8,ec3` on
    a run a `hevc8,aac51,ec3` session started gets AAC 5.1 it did not
    declare (measured: `av_eac3_multi.mkv`, master `CHANNELS="6"`,
    output AAC 5.1), and in the other order (not measured) the `aac51`
    session gets stereo. The master says what the run makes (`runHLS`).
    Left as it is (review finding 6): putting what a fallback would make
    into the variant splits runs these declarations share today.
- **Seeks.** A copied AAC 5.1 on the re-encode route is cut at the seek point
  like copied stereo (`reencodeSeekCuts`); passthrough cuts every audio
  output, copied Dolby included (`passthroughAudioMaps`); the copy route cuts
  none. Measured on 8.1.2 (`e2e/audio/avsync_audio.py`, the served segments
  as hls.js places them; positive: audio late; x265/x264 video with the
  83 ms B-frame delay, libfdk_aac sources with 43 ms priming):

  | Route, audio | A/V from the start | After a seek to 35 | Today (no tokens), after the seek |
  |---|---|---|---|
  | re-encode, AAC 5.1 copied (`aac51`) | −40 ms | −83 ms (audio at its movie time, 0.0 ms; the video 83 ms late) | −41 ms (AAC stereo encode) |
  | same, without the cut | | +10 162 ms | |
  | re-encode, E-AC-3 / FLAC 7.1 → AAC 5.1 | −41 ms | −41 ms | −41 ms |
  | passthrough, E-AC-3 copied (`ec3`) | −78 ms | −14 ms | +43 ms (AAC stereo) |
  | passthrough, AC-3 copied (`ac3`) | −78 ms | −14 ms | |
  | passthrough, AAC 5.1 copied | −40 ms | −8 ms | |
  | passthrough, E-AC-3 → AAC 5.1 | −40 ms | +43 ms | +43 ms |
  | passthrough, DTS → AAC 5.1 | −30 ms | +54 ms | +54 ms (the DTS decoder's delay) |
  | copy, AAC 5.1 copied | −40 ms | −8 ms | −8 ms |

  From the start passthrough's error is the video's B-frame delay (hls.js
  ignores the edit list) against the audio's own delay: AAC's priming hides
  half of it, E-AC-3's 256 samples (5.3 ms) do not. After a seek a copy is
  early by less than one audio frame (32 ms for E-AC-3, 21 ms for AAC at
  48 kHz): the cut keeps the first packet at or after the zero, movenc
  starts the track's `tfdt` at that packet (`mov_write_tfdt_tag`,
  `cluster[0].dts - start_dts`, `start_dts` the first packet's DTS) and puts
  the difference in the edit list, which hls.js ignores — the mechanism of
  copied stereo AAC's −8 ms.
- **Checked in Chrome 154 with hls.js 1.6.14** (`e2e/passthrough/page`,
  `channels()`): AAC 5.1 in TS (re-encode route) and in fMP4 (passthrough),
  copied and encoded, plays before and after a seek with no hls.js error or
  stall; SourceBuffers `audio/mp4;codecs=mp4a.40.2`; WebAudio gets 6
  channels, the source's silent LFE silent (index 3), the 7.1 source's front
  channels at −4.6 dB. Chrome answers no to `ec-3` and `ac-3`, so a copied
  Dolby track is not played there; that check is left to Safari and Edge.
- **Not verified:** TrueHD 7.1 and E-AC-3 7.1 sources (FFmpeg's encoders stop
  at 5.1; the 7.1 downmix was checked on FLAC 7.1, the same decoded layout),
  a real E-AC-3 JOC (Atmos) stream, a real (not crafted) E-AC-3 with more
  than one independent substream, an AC-3 with bsid over 8 (movenc refuses
  its `dac3`; no such source could be made: FFmpeg does not decode a
  patched one), native HLS players and their use of CHANNELS.

### Capabilities (GET /capabilities)

`GET /capabilities` (and `HEAD`; other methods 405) answers what a session
opened **now** passes through:

```json
{"passthrough_video_codecs":["hevc"]}
```

- The value is `passthroughCapability` exactly as POST /session reads it:
  the flag, or the capability file re-read when it changed. An empty
  capability is `[]`, never `null` — a client that treats the key as "the
  transcoder answered" reads "none", not "no answer".
- It says whether passthrough **exists** here, not which route a given
  session will get: that is still decided per session from the source and
  the client's declaration (`videoRouteFor`).
- For services, not browsers: no CORS headers, `Cache-Control: no-store`.
  It touches no session, run, output directory or metric; its cost is the
  `stat` of the capability file that every POST /session also does.
- Pods read the same ConfigMap, but each sees a change when the kubelet
  syncs its mount (typically 1–2 min, not measured here): for that long
  pods behind one Service may answer differently.
- web-ui polls it in the background for Discover; a transcoder without
  this endpoint (404) is "no answer" there, never "none".

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

- **Segments (`.ts`, `.vtt`, `.m4s`)** carry `ETag: "<generation>-<size hex>"`. The generation is a random name given to each FFmpeg process of a run (`TranscodeRun.generation`, made for each process before its arguments). A passthrough init carries the generation in its name, and its `ETag` names that one.
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
5. Inject `#EXT-X-SESSION-OFFSET:<real start>` — movie-time of segment 0 in this variant: the quantized seek, or, when the video is copied, where FFmpeg's seek landed (see [FFmpeg Seek Strategy](#ffmpeg-seek-strategy)). Read by downstream proxies (THP grace-window math) and ignored by players per RFC 8216 §3.1
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

A `TranscodeRun` is one FFmpeg process writing segments to `{hashDir}/runs/seek-{time}/` (a passthrough run: `{hashDir}/runs/hevc-seek-{time}/`). It is reference-counted — multiple sessions can share it.

### Run Identity

Runs are keyed by `(hashDir, seekTime)`: `{hashDir}:seek:{t}`. Two sessions with the same source URL and same quantized seek time share the same run. A passthrough run is keyed `{hashDir}:hevc:seek:{t}` (`runKeyFor`): a passthrough and an old-route session of the same source never share a run, a directory, a remembered real start (`ResolvedStart`) or remembered FFmpeg options (`fallbackKey`). A session whose declaration changes an audio output has the audio variant in its key (`{hashDir}:a6c:seek:{t}`, `{hashDir}:hevc-accc6666:seek:{t}`; see [Audio](#audio-multichannel-aac-and-dolby)). The old route's key, directory and options are the ones it always had, so the old and new pods of a rollout keep sharing its runs.

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

**Passthrough runs are paced in media time** (`services/pacing_media.go`). Their video is copied and cut at its keyframes (10 s for a typical x265 GOP) while the audio is cut every 4 s; counted by segment numbers, the audio's numbers would set the demand and the run would freeze at video segment `(t+30)/4 + 75` — 14 min ahead at the start, an hour at t = 30 min. So for them a request for segment n of a stream is a viewer at that segment's start (the EXTINF sum before it in that stream's playlist; a segment not listed yet is a viewer at the stream's edge), production is the EXTINF sum of the primary playlist, and the run freezes at production ≥ demand + lead and continues below demand + lead − 60 s. A resume counts as stalled after 60 s without a new primary segment (provisional), under `mode="passthrough"`; an alert that sums `run_resume_stalls_total` over every mode counts these too, so it should filter `mode!="passthrough"` until passthrough has a threshold of its own. The old route's runs keep the segment pacing above.

## FFmpeg Seek Strategy

### Copy Mode (h264 source → `-c:v copy`)

```
ffmpeg -ss {time} -noaccurate_seek [-itsoffset {time - realStart}] -i {url} ... -c:v copy ... -ss 0 -map 0:{subtitle} ... -c:s webvtt ...
```

- `-ss` before `-i`: fast input-level seek (keyframe-based)
- `-noaccurate_seek`: disables frame trimming between keyframe and target. Both video (copy) and audio (re-encode) start from the **same keyframe** → perfect A/V sync
- Segments numbered from 0, PTS from 0
- **The offset is where FFmpeg's seek lands** (`realStart`: `#EXT-X-SESSION-OFFSET`, the seek answer's `offset`): the movie time of the run's first frame. It is resolved before the run starts by `ffmpegSeekFirstFrame`: FFmpeg with the run's own seek options (`copySeekInput`, shared by the run and the probe, and by passthrough's) plus `-frames:v 1 -f framecrc`; the answer is the first packet's PTS. Until 2026-09 the copy route asked ffprobe (`-read_intervals T%+#1`), whose seek has no dts heuristic and ignores the file's start time. Measured on 8.1.2 (24 fps MKVs, two frames of B-frame delay):

  | Source, seek | ffprobe (before) | FFmpeg (now) | The run's first frame |
  |---|---|---|---|
  | 10 s GOPs, seek 30 | 30.000 | 20.000 | 20.000 |
  | keyframe at 29.917, seek 30 | 29.917 | 20.000 | 20.000 |
  | keyframe at 59.833, seek 60 | 59.833 | 59.833 | 59.833 |
  | start_time 5, seek 30 | 25.000 | 20.000 | 20.000 |
  | no B-frames, seek 30 | 30.000 | 30.000 | 30.000 |
  | keyframes at 0 and 30, seek 30 | 30.000 | 0.000 | 0.000 |

  Modelled on the Cues of 89 production sources, about 10% of copy-route seeks were 1.4–10.4 s off.
- **The answer can be after the seek point.** A format without an index (MPEG-TS) seeks to a byte position by the timestamps it finds there, and the copy drops what comes before the next keyframe (`ffmpeg_mux.c`, `of_streamcopy`). Measured on 8.1.2, a TS with keyframes at 5, 15, 25, 35 s: a seek to 30 starts the run at 35.021, one to 60 at 65.021 — and the probe says so, since it seeks exactly as the run does. The guard (`resolveRealStart`) takes an answer up to 60 s after the seek point on this route (passthrough keeps refusing any: its `-itsoffset` only moves the zero back); before, it replaced it with the quantized time, and the offset was 5 s before the picture. In production 15 of 2229 copy-route seek starts in 3 days were `.ts` (0.7%).
- **A keyframe at the file's first frame can read a hair below zero.** FFmpeg counts timestamps from the seek point by rescaling it to the stream's time base, to the nearest tick (`ffmpeg_demux.c`, `ts_fixup`): 60 s in an AVI's 1001/24000 is 1438.56 ticks, rounded to 1439, so the first frame read −0.018 from a seek to 60 and the guard reported 60.000 for a run starting at 0. An answer under zero by less than a tick is zero (`ffmpegSeekFirstFrame`).
- **The PTS, not the DTS.** The run's video output puts its zero at the keyframe's DTS (the segment muxer shifts its first negative DTS to 0; the first frame's PTS in the TS is the B-frame delay, 0.083 s). hls.js places TS video by its PTS and plays the first one at media time 0: measured in Chrome 154 with hls.js 1.6.14, the frame at movie 20.000 played at 0.000. So the offset is the first frame's movie time; the DTS would put every side-loaded cue that far late. Passthrough's fMP4 keeps the DTS (`ffmpegSeekStart`), which its `-itsoffset` counts from.
- **Every output counts from `realStart`** (`injectCopySeekParams`): `-itsoffset {time − realStart}` among the input options, as passthrough does, and `-ss 0` on each subtitle output. Without it every output took the quantized time as zero and shifted its own negative timestamps away (`libavformat/mux.c`, `avoid_negative_ts`): the video and the audio began at the keyframe the offset names, the subtitles at the first cue the demuxer handed over — every cue early against the offset by the distance from the keyframe to that cue (up to a GOP), in hls.js and in subtitle-translate's cue + offset alike. `-ss 0` on a subtitle output drops the cues that start before the zero, encoded (`ffmpeg_enc.c`, `do_subtitle_out`) or copied WebVTT (`of_streamcopy`), and shifts neither; those are the cues before the keyframe (MKV hands over subtitle blocks from before it, mov_text in MP4 seeks to the cue on screen), which would otherwise be negative and move all the rest. The one on screen at the keyframe is lost with them. Measured on 8.1.2, SRT cues at 1, 21, 26, 28, 33, 41, 61 s:

  | Source, seek | Offset | Cues before (cue + offset − movie) | Now |
  |---|---|---|---|
  | keyframes every 10 s, seek 35 | 20.000 | −1 s each (21 s served at 0.000) | 0, from 21 s on |
  | keyframes at 0 and 30, seek 35 | 0.000 | −1 s each (1 s served at 0.000) | 0, from 1 s on |

  ASS, copied WebVTT and mov_text the same (MP4 lands on 30 there: no offset, the cut drops the 28–32 s cue). In Chrome 154 with hls.js 1.6.14 (e2e/seek/page, `avs_gap7_h264.mkv`, seek 35): every cue −0.083 s against the offset, as from the start (hls.js puts WebVTT time 0 at TS time 0, and the video's first frame is at the B-frame delay); before, −1.083 s; production +8.917 s (its offset was 30.000). The video and the audio are unchanged by it: the offset is the video's first PTS, so its first DTS is still negative by the B-frame delay and its output shifts that away as it shifted the larger one before; the audio, which the demuxer hands over from a little before the keyframe (the first AAC packet at 19.925 for the keyframe at 20.000), likewise. Every video and audio segment and playlist came out byte for byte the same with and without it on those sources. Where the first audio packet after the seek comes after the keyframe (a copied track starting later than the video, or no B-frames) the audio changes: its output no longer shifts to zero and counts from `realStart`, as in the run from the start (measured: a track starting 0.5 s after the video used to play 0.479 s early; a run landing on 0 now serves the run from the start's audio byte for byte). In a mixed rollout those audio segments differ between an old and a new pod too, not only the subtitles.
- **Without a probe answer** (the probe failed or answered implausibly, `transcoder_run_real_start_total{result!="ok"}`, about 10% of copy seek starts over 3 days with 1b25e28's ffprobe probe, nearly all its 5 s timeout; the FFmpeg probe's rate is not measured yet) the run is the input seek alone: no `-itsoffset` and no subtitle cut, the argv from before them. The run's zero is not known then, and a cut at the quantized seek dropped every cue between the keyframe and it and served the rest early by that distance (10 s on a 10 s GOP) where the plain argv serves them 1.083 s early and keeps them. The copy route keeps even such a fallback for the key (the offset must not move under a session), and keeps with it that it was one: a run re-created from that memory does not cut either.
- **A seek run can answer offset 0.000**: when the only keyframe before the seek point is the file's first (or the first within FFmpeg's 3/23 s heuristic before it, and none earlier). subtitle-translate takes offset 0 as the run from the start: it keys the run by offset and name, and only a contiguous offset-0 run writes the final translation, shared by every session of the file and kept in S3 for good. With every output counted from `realStart`, such a run's subtitle playlist and segments are byte for byte the run from the start's (measured, and held by `TestCopyRoute_RealFFmpegCues` and e2e `copy_cues_first_keyframe`); before, its cues were early by the first cue's time (1 s on the e2e source), and a translation made from it would have been stored early. subtitle-translate has no test for a seek run answering 0; the contract it relies on is this one.
- **Only a zero moved back.** When the run starts after the seek point (MPEG-TS), there is no `-itsoffset`: the demuxer hands over the audio from before the keyframe (the first AAC packet at 29.739 for a seek to 30 that started the video at 35.021), and a zero moved forward to the keyframe made that audio negative — its output shifted the whole track, 5.2 s late against the picture (measured). The subtitle cut still keeps cues from before the seek point from moving the rest; the cues run late against the offset by the distance to the keyframe, as before. The subtitle streams an MPEG-TS usually carries (DVB, teletext, ARIB) get no output on this route (`textSubtitleCodecs`); an MKV without an index whose seek lands after the seek point would have its cues late the same way (not measured).
- **Known, not changed here:**
  - The copied audio of such a TS seek run is 261 ms late by its timestamps (computed from the served segments, not played): its first packet is 0.261 s before the seek point, its output shifts it to zero, and the video's first timestamp, positive, is not shifted. Same in production (these arguments are unchanged); cutting the audio at the seek point (`-ss 0`) looks like the fix, not tried.
  - The offset is counted in the file's time (from the format's start_time), and so are the cues. When the video starts after the file (an MPEG-TS whose AAC starts with priming: 21 ms; a remux with the video 0.5 s late), hls.js plays the run from the start with media time 0 at the video's first frame, so that run's picture is behind the file's time by the video's start, while its cues are not. A seek run's picture and cues are both on the file's time. Subtracting the video's start from the seek offset would line the seek run's picture up with the run from the start's but move its cues off the run from the start's in subtitle-translate's cue + offset. Same in production.

### Re-encode Mode (mpeg4, vp9, etc. → `-c:v h264`)

```
ffmpeg -ss {time} -i {url} ... -c:v h264 -preset veryfast ... -ss 0 -map 0:{copied audio} ... -c:a copy ... -ss 0 -map 0:{subtitle} ... -c:s webvtt ...
```

- `-ss` before `-i` here too (`injectSeekParams` places it there in both modes). FFmpeg seeks the input to the keyframe before `{time}` using the container index, then decodes and discards frames up to `{time}` (accurate seek is the default).
- **The accurate seek trims only decoded streams.** The trim is a filter at the input of a stream's filter graph (`fftools/ffmpeg_filter.c`, `insert_trim`). The video and every encoded audio track start exactly at `{time}`. A copied AAC track has no graph: it starts at the keyframe the demuxer landed on, up to a GOP earlier (for an MKV with B-frames, at or before `{time}` − 3/23 s, `ffmpeg_demux.c` `dts_heuristic`). Its output is a segment muxer of its own, which shifts its first negative timestamp to zero (`libavformat/mux.c`, `avoid_negative_ts`), so audio and video no longer share a clock.
- **`-ss 0` on each copied audio output** (`cutAtOutputStart`, `HLS.reencodeSeekCuts`). An output start time of 0 drops the copied packets whose DTS is before `{time}` (`ffmpeg_mux.c`, `of_streamcopy`). Measured on FFmpeg 8.1.2, a seek to 35 (run at 30) on a 10 s GOP MKV, copied AAC:

  | | A/V (positive: audio late) | Audio playlist (video 40.08 s) |
  |---|---|---|
  | From the start | −62 ms | — |
  | After the seek, without the cut | +10 162 ms | 50.26 s |
  | After the seek, with the cut | −83 ms | 40.01 s |

  hls.js places the audio by its PTS against the video's, so without the cut the sound played 10 s late for the whole run, and the media ended 10 s after the picture. Encoded tracks (AC3, 5.1 AAC without `aac51`, `EncodeAudio`) are left alone: the trim already cuts them. A 5.1 AAC copied for `aac51` is cut like the stereo one (the same decision, `audioOutputFor`): measured −83 ms after the seek, the audio at its movie time; +10 162 ms with the cut taken off it.
- **`-ss 0` on each subtitle output** too. Subtitles never go through a filter graph (encoded to webvtt or copied), and matroskadec does not skip subtitle blocks before the keyframe it seeks to (`skip_to_keyframe` is for the other tracks). The cues between where the demuxer landed and `{time}` came out with negative times, and the output shifted them to zero like the audio's, so every later cue ran late against `#EXT-X-SESSION-OFFSET` (which is `{time}` on this route). Measured on 8.1.2, a seek to 35 (run at 30), cues at 21, 26, 33 s: served at 0.000, 5.000, 12.000 (the 33 s cue 9 s late, in hls.js and in subtitle-translate's cue + offset alike); with the cut, the cue at 33 s at 3.000 and nothing from before 30 s. With the cut an encoded cue that starts before `{time}` is dropped (`ffmpeg_enc.c`, `do_subtitle_out`), a copied webvtt one like copied audio; a cue still on screen at `{time}` is lost with it (FFmpeg compares the cue's start). That costs more for long cues — ASS signs and songs, forced subtitles — than for a typical 2–5 s line; `-ss` on an output does not trim a cue's start in FFmpeg 8.1.2, only drops or keeps it. The error was content-dependent: up to a GOP, and only when a cue fell between the landing keyframe and `{time}`.
- On any seek (`{time}` > 0), `-xerror` is removed. AVI and other containers report non-fatal errors after a seek, and `-xerror` would turn them into a failed run.
- From the start (`{time}` = 0) `-xerror` stays. Without it, a failed read of the source ends FFmpeg like the end of the file: exit 0, a completed run, and it is never restarted.
- Per-source fallbacks (`ParamOptions`, remembered by the RunManager per source). When a run dies on a failure a known option cures, that source's later runs get the option:
  - timestamps (`Non-monotonic DTS` / `Invalid DTS` under `-xerror`) → `Lenient`, which drops `-xerror`;
  - `Scalable configurations are not allowed in ADTS` → `EncodeAudio`, which re-encodes AAC the probe would copy;
  - a muxer refusing a packet of a copied audio output (`[aost#…/copy @ …] Error submitting a packet to the muxer`), only when the session copies audio because the client declared it (`copiesDeclaredAudio`) → `EncodeAudio` and `Lenient`, on seek runs too (see [Audio](#audio-multichannel-aac-and-dolby), Fallback).

### Passthrough Mode (HEVC → fMP4)

```
ffmpeg -ss {quantized} -noaccurate_seek -itsoffset {quantized - realStart} -i {url} ... -c:v copy ... -ss 0 -map 0:{audio} ...
```

- The input seek is the copy route's, at the quantized time, counted from
  the file's start time like every other run's: the seek run and the run
  from 0 share one timeline whatever the file's `start_time` (a source
  remuxed with start_time 5: a seek to 30 lands on the keyframe at movie
  20.000, offset 19.917 — with `-seek_timestamp 1`, which the first
  version had, the offset read 24.917).
- **Not `-ss {realStart}`.** FFmpeg's input seek goes back to the keyframe
  at or before the target. For a format without `AVFMT_SEEK_TO_PTS`
  (matroska) that has B-frames, it first takes 3/23 s off the target
  (`fftools/ffmpeg_demux.c`, `dts_heuristic`). So `-ss` at the keyframe
  itself lands a whole GOP earlier. Measured on 8.1.2 with 10 s GOPs:
  `-ss 20.020` on an MKV keyframe at 20.020, and `-ss 19.770` on an MP4
  keyframe with that DTS, both started at 10.010.
- **`realStart`** is resolved with FFmpeg itself (`ffmpegSeekStart`), not
  ffprobe. It uses the run's seek options plus `-frames:v 1 -f framecrc`,
  whose timestamps come relative to the seek point as the run's do; the
  real start is the quantized time plus the earlier of the first packet's
  DTS and PTS (−10.083 for a seek to 30 over the keyframe at movie 20.000,
  DTS 19.917, on the source and on its start_time 5 copy alike). ffprobe's
  seek has no dts heuristic, so it names a keyframe the run does not start
  at whenever one lies in the last 3/23 s before the seek point. Example: a
  seek to 30 on a 25 fps, 10 s GOP MKV. ffprobe says 30.000, FFmpeg starts
  at 20.000. ffprobe also has no DTS for matroska, where FFmpeg guesses
  one: 19.937 for a keyframe at 20.020 with two frames of B-frame delay.
- **`-itsoffset`** moves every output's zero from the quantized time to
  `realStart`. The video's first DTS is 0, so its output shifts nothing, and
  subtitles count from the same zero. Measured on 8.1.2, cues at 21 s and
  26 s after a seek to 30 over that MKV keyframe:
  - with the offset: 1.063 and 6.063;
  - without it: 0.000 and 5.000 (each output shifts its own negative
    timestamps away).
- **`-ss 0` on every audio output** (only with a resolved `realStart`).
  The demuxer's seek lands the audio a little before the video's keyframe
  (the first copied AAC packet 162 ms before `realStart` on the e2e A/V
  source), and each output shifts its own negative timestamps to zero, so
  the audio played that much late after every seek. An output start time
  of 0 drops what comes before it: a copied packet whose DTS is below it
  (`ffmpeg_mux.c`, `of_streamcopy`), encoded samples through a trim
  (`ffmpeg_filter.c`, `insert_trim`). Measured through the hls muxer, in
  a player that ignores edit lists (positive: audio late):

  | Audio | From the start | After a seek, before | After a seek, now |
  |---|---|---|---|
  | AAC copied | −62 ms | +162 ms | −8 ms |
  | AC3 encoded (libfdk_aac) | −40 ms | +184 ms | +43 ms |

  From the start the error is the video's B-frame delay (its first PTS is
  83 ms, hls.js ignores the edit list) against the audio's priming, as on
  the TS routes. Without a resolved `realStart` the audio is left whole:
  the video then starts at the keyframe before the zero, and audio cut at
  the zero would run ahead of it by up to a GOP.
- **If the probe does not answer** (or names an implausible keyframe),
  `realStart` is the quantized time, there is no offset and no audio cut:
  the copy route's behaviour. Unlike the copy route, a passthrough run
  keeps no such fallback for its key (`RunManager.realStarts`): the next
  run of the key probes again. Kept, one probe timing out on a cold source
  fixed that key's offset — up to a GOP off, subtitles with it — for the
  pod's life. Every probe is counted in
  `run_real_start_total{mode,result}`.
- **Audio CODECS.** The master says `mp4a.40.2` for any AAC, `ec-3` /
  `ac-3` for copied Dolby. hls.js 1.6.14
  builds its SourceBuffers from the codecs in the init segment
  (`passthrough-remuxer.ts`, `getParsedTrackCodec`), so a copied HE-AAC
  track is declared by its own `esds` there; what a native HLS player does
  with the mismatch is not verified (browser matrix).
- **Copied E-AC-3 / AC-3** are cut at the zero like any audio output. movenc
  merges E-AC-3 frames to 6 blocks per sample and writes `dec3` (with the
  JOC extension of an Atmos stream) or `dac3` from the packets it has seen,
  which hlsenc's `delay_moov` guarantees before the init is written.
  Measured: −14 ms after a seek (see [Audio](#audio-multichannel-aac-and-dolby)).

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
      a6c-seek-0.000/              # A declaration that changes the audio: its own runs
        ...
      hevc-seek-0.000/             # Passthrough run from 0s
        v0-2160-init-{gen}.mp4     # Video init of process {gen} (one per process)
        v0-2160-0.m4s              # Video segments (cut at keyframes)
        a0-init-{gen}.mp4, a0-0.m4s  # Audio, fMP4 too
        s0-0.vtt                   # Subtitles as on the old route
        v0-2160.m3u8.ffmpeg        # hlsenc's playlist (#EXT-X-MAP names the init)
        ffmpeg.out, ffmpeg.err
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
| `video_route_total{route,reason}` | POST /session answers by route (`passthrough`, `copy`, `reencode`, `audio`; `refused` for the 415 of a video the route would have to encode or the 503 of a failed check, `error` for any other failure — nothing playable among them) and reason (every reason is registered on `reencode` and `refused` from the start; `no_hevc_declared` since the audio tokens) |
| `run_real_start_total{mode,result}` | Probes of where a copy or passthrough seek run really starts: `ok`, `failed` (error, timeout), `implausible` (before the file, over 60 s before the seek, or after it: over 60 s on the copy route, at all on passthrough). Not `ok`: the run reports the quantized seek |
| `source_probe_seconds{result}` | The passthrough source probe, retries included, `ok`/`failed`; cached results excluded |
| `session_segments_served{route}` | Primary segments served to a session, observed when it is removed (sessions that never started a run are not observed) |
| `passthrough_codecs_mismatch_total{field}` | Passthrough masters whose output hvcC differs from what the route was decided on — the source's parameter sets as `outputHVCC` merges them (`profile`, `tier`, `level`) — or whose init gave no CODECS (`unbuildable`, master refused). Expected 0 |

Run metrics with a `mode` label (`run_first_segment_seconds`, `run_speed`,
`run_pause_seconds_total`, `run_resume_segment_seconds`,
`run_resume_stalls_total`) have `mode="passthrough"` for passthrough runs.

A run's outcome is recorded once, when the process is reaped
(`TranscodeRun.reapProcess`): whoever stops it leaves the reason in
`stopReason` first, so a process that dies on its own (`failed`) is told
apart from one we ended. `failed` + `signal` is an OOM kill or the node;
`failed` + `error` is FFmpeg giving up on the source.
