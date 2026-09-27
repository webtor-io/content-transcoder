#!/bin/sh
# Seeks on the old route end to end, with the transcoder image's real
# FFmpeg: the image under test (E2E_NEW_IMAGE, built from this checkout
# unless E2E_BUILD=0) against the image in production (E2E_OLD_IMAGE),
# which must show the bugs the new one fixes. Reuses the passthrough
# harness (../passthrough: ctl.sh, lib.py, mediasrv, the ffmpeg/ffprobe
# wrappers) under its own container prefix (E2E_PREFIX, default ctsk) and
# work dir (E2E_WORK, default ./work). Steps:
#
#   seek     seek.py against both images: A/V, cues and the copy route's
#            offset after a seek (limits in seek.py); the new image must
#            pass, the production image must fail
#   gotest   TestCopyRoute_RealFFmpeg* (services/run_start_ffmpeg_test.go:
#            the offset, the cues, the probe's tick rounding) in the
#            production image's FFmpeg, on gen.sh's sources
#   golden   the old route's golden records (../passthrough/golden.py) of
#            both images differ only where the fixes mean them to
#            (golden_expect.py); needs the passthrough sources (its gen.sh)
#            in E2E_PT_WORK (default ../passthrough/work)
#
# Ports 18680 (new) and 18780 (production); golden uses the passthrough
# harness's 18080/18180/18280.
#
# Browser: serve page/ (python3 -m http.server 18091 --bind 127.0.0.1
# --directory page), open it in Chrome, click the button, then in the
# console `await check('http://127.0.0.1:18680', 'avs_gap7_hevc.mkv',
# {seek: 35, seconds: 16})` (and avs_gap7_h264.mkv; 18780 for production).
#
#   run.sh [seek|gotest|golden ...]   (default: seek gotest)
set -e
D=$(cd "$(dirname "$0")" && pwd)
PT=$D/../passthrough
export E2E_PREFIX=${E2E_PREFIX:-ctsk}
export E2E_WORK=${E2E_WORK:-$D/work}
export E2E_TOOLS_DIR=$D
export E2E_NEW_IMAGE=${E2E_NEW_IMAGE:-ct-seekfix:local}
export E2E_OLD_IMAGE=${E2E_OLD_IMAGE:-ghcr.io/webtor-io/content-transcoder:sha-1b25e28}
W=$E2E_WORK
P=$E2E_PREFIX
NEW=18680
OLD=18780
cd "$D"
mkdir -p "$W"

if [ "${E2E_BUILD:-1}" = 1 ]; then docker build -q -t "$E2E_NEW_IMAGE" "$D/../.." >/dev/null; fi
"$PT/ctl.sh" tools
"$PT/ctl.sh" media
docker exec "$P-tools" sh /e2e/gen.sh >/dev/null 2>&1

steps=${*:-seek gotest}
fail=0
for step in $steps; do
  echo "== $step"
  case $step in
  seek)
    "$PT/ctl.sh" up new "$E2E_NEW_IMAGE" $NEW
    "$PT/ctl.sh" up old "$E2E_OLD_IMAGE" $OLD
    python3 seek.py http://127.0.0.1:$NEW "$W/new.json" > "$W/new.log" 2>&1 || true
    python3 seek.py http://127.0.0.1:$OLD "$W/old.json" > "$W/old.log" 2>&1 || true
    python3 seek.py --judge "$W/new.json" > "$W/new.judge" || { echo "FAIL new image:"; cat "$W/new.judge"; fail=1; }
    tail -1 "$W/new.judge"
    # Negative control: production has the bugs.
    if python3 seek.py --judge "$W/old.json" > "$W/old.judge"; then
      echo "FAIL negative control: the production image passes"; fail=1
    else
      echo "negative control: the production image fails, as it must ($(grep -c '^FAIL' "$W/old.judge") failures)"
    fi
    ;;
  gotest)
    arch=$(docker version --format '{{.Server.Arch}}')
    (cd "$D/../.." && GOOS=linux GOARCH=$arch CGO_ENABLED=0 go test -c -o "$W/ct.test" ./services)
    docker run --rm -v "$W:/w" -e COPY_SEEK_MEDIA=/w/media --entrypoint /w/ct.test "$E2E_OLD_IMAGE" \
      -test.run TestCopyRoute_RealFFmpeg -test.v > "$W/gotest.log" 2>&1 || fail=1
    grep -E '^(--- |    --- )' "$W/gotest.log"
    ;;
  golden)
    PW=${E2E_PT_WORK:-$PT/work}
    E2E_PREFIX=${P}g E2E_WORK=$PW "$PT/ctl.sh" media
    for s in old off_nodecl cap_nodecl; do
      (cd "$PT" && E2E_PREFIX=${P}g E2E_WORK=$PW python3 golden.py record $s "$W/golden_$s.json" > "$W/golden_$s.log" 2>&1) || fail=1
    done
    # Its containers hold the passthrough harness's ports: gone when done.
    E2E_PREFIX=${P}g E2E_WORK=$PW "$PT/ctl.sh" down
    for s in off_nodecl cap_nodecl; do
      python3 golden_expect.py "$W/golden_old.json" "$W/golden_$s.json" > "$W/golden_$s.expect" || fail=1
      tail -1 "$W/golden_$s.expect"
    done
    ;;
  esac
done
[ $fail = 0 ] && echo "ALL PASS" || echo "SOMETHING FAILED"
exit $fail
