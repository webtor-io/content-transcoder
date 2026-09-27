#!/bin/sh
# HEVC passthrough end to end, with the transcoder image's real FFmpeg.
#
# Sources are made with that FFmpeg (gen.sh) and served over HTTP by a small
# file server (mediasrv, Range and throttling). Transcoders run as docker
# containers from the image under test (E2E_NEW_IMAGE, built from this
# checkout unless E2E_BUILD=0) and from the image in production
# (E2E_OLD_IMAGE), their ffmpeg/ffprobe wrapped to record every call. Steps:
#
#   cases    passthrough sessions played like a player (master, variant,
#            init, every segment, audio, subtitles, a seek) and checked with
#            ffprobe; the route decision and its answers (200/415 and reason)
#   golden   the old route: production image against the new one with no
#            passthrough (capability empty; no, unknown or garbage decode;
#            declarations the route turns down), byte for byte: calls,
#            answers, playlists, segments, legacy GET, metric families; and
#            a negative control with passthrough on, which must differ
#   pacing   where a passthrough run freezes against a viewer (media time),
#            and the old copy route on the production image for comparison
#   avsync   A/V and timeline error per route, from the start and after a
#            seek, on a source with a flash and a click every second
#
# Needs docker, go, python3 (standard library only). Work dir: E2E_WORK
# (default ./work). Ports 18080 (capability hevc), 18180 (none), 18280
# (production image), each with metrics on +3.
#
# Browser: after `run.sh cases`, serve page/ (python3 -m http.server 18090
# --bind 127.0.0.1 --directory page) and in Chrome on 127.0.0.1:18090 call
# `await run('main8_1080.mkv', {seek: 35, seconds: 8})` in the console
# (hls.js from the CDN; tokens from isTypeSupported/decodingInfo).
#
#   run.sh [cases|golden|pacing|avsync ...]   (default: all)
set -e
D=$(cd "$(dirname "$0")" && pwd)
export E2E_WORK=${E2E_WORK:-$D/work}
W=$E2E_WORK
export E2E_NEW_IMAGE=${E2E_NEW_IMAGE:-ct-e2e:local}
export E2E_OLD_IMAGE=${E2E_OLD_IMAGE:-ghcr.io/webtor-io/content-transcoder:sha-1b25e28}
cd "$D"
mkdir -p "$W/golden" "$W/pacing"

if [ "${E2E_BUILD:-1}" = 1 ]; then docker build -q -t "$E2E_NEW_IMAGE" "$D/../.." >/dev/null; fi
./ctl.sh tools
./ctl.sh media
docker exec cte2e-tools sh /e2e/gen.sh >/dev/null
[ -f "$W/media/dv5_1080.mp4" ] || python3 craft.py "$W/media/hevc10_1080_plain.mp4" "$W/media/dv5_1080.mp4" 5 6 0
[ -f "$W/media/dv5_2160.mp4" ] || python3 craft.py "$W/media/hevc10_2160_plain.mp4" "$W/media/dv5_2160.mp4" 5 9 0

steps=${*:-cases golden pacing avsync}
fail=0
for step in $steps; do
  echo "== $step"
  case $step in
  cases)
    ./ctl.sh up new-cap "$E2E_NEW_IMAGE" 18080 PASSTHROUGH_VIDEO_CODECS=hevc
    ./ctl.sh up new-off "$E2E_NEW_IMAGE" 18180
    python3 cases.py "$W/cases.json" || fail=1
    ;;
  golden)
    for s in old old_decl off_nodecl off_full cap_nodecl cap_unknown cap_garbage cap_short cap_full; do
      python3 golden.py record $s "$W/golden/$s.json" > "$W/golden/$s.log"
    done
    for s in old_decl off_nodecl off_full cap_nodecl cap_unknown cap_garbage; do
      python3 golden.py compare "$W/golden/old.json" "$W/golden/$s.json" || fail=1
    done
    # Declarations turned down after the transcoder's own look: the same
    # old route, plus that look.
    python3 golden.py compare "$W/golden/old.json" "$W/golden/cap_short.json" --drop-source-probe || fail=1
    # Negative control: passthrough on must not compare equal.
    if python3 golden.py compare "$W/golden/old.json" "$W/golden/cap_full.json" > "$W/golden/cap_full.compare"; then
      echo "FAIL negative control: passthrough on compares equal to the old route"; fail=1
    else
      echo "negative control: passthrough on differs, as it must ($(grep -c DIFF "$W/golden/cap_full.compare") items)"
    fi
    ;;
  pacing)
    ./ctl.sh up new-cap "$E2E_NEW_IMAGE" 18080 PASSTHROUGH_VIDEO_CODECS=hevc
    python3 pacing.py slow/2048/long_gop10.mkv hevc8 cte2e-new-cap 18080 "$W/pacing/passthrough.json" || fail=1
    ./ctl.sh up old "$E2E_OLD_IMAGE" 18280
    python3 pacing.py slow/8192/long_h264_gop10.mkv - cte2e-old 18280 "$W/pacing/old_copy.json"
    ;;
  avsync)
    ./ctl.sh up new-cap "$E2E_NEW_IMAGE" 18080 PASSTHROUGH_VIDEO_CODECS=hevc
    python3 avsync.py "$W/avsync.json"
    ;;
  esac
done
[ $fail = 0 ] && echo "ALL PASS" || echo "SOMETHING FAILED"
exit $fail
