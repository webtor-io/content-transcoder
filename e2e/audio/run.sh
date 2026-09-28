#!/bin/sh
# Multichannel audio (decode tokens aac51, ac3, ec3) end to end, with the
# production image's FFmpeg: the transcoder built from this checkout runs
# inside the production image (E2E_BASE_IMAGE), whose FFmpeg made the
# sources (gen.sh) too. Reuses the passthrough harness (../passthrough:
# ctl.sh, lib.py, avsync.py, mediasrv, the ffmpeg/ffprobe wrappers) under
# its own container prefix (E2E_PREFIX, default aud) and work dir
# (E2E_WORK, default ./work). Steps:
#
#   gotest   TestAudio_RealFFmpeg (services/audio_ffmpeg_test.go) in the
#            production image: codec, channels, layout, rate, fMP4 sample
#            entries, CODECS and CHANNELS, the seek cuts
#   avsync   A/V after a seek to 35 (and from the start) on the route and
#            audio each declaration gives, from the served segments
#            (avsync.py's measurement; limits in avsync_audio.py)
#   layout   where each input channel of a 5.1(side) and a 7.1 source ends
#            up in the AAC 5.1 the transcoder encodes, and at what level
#            (layout.py)
#
# Port 18480 (capability hevc; metrics on 18483).
#
# Browser (Chrome, hls.js 1.6.14 from the CDN): serve ../passthrough/page
# (python3 -m http.server 18092 --bind 127.0.0.1 --directory
# ../passthrough/page) and open http://127.0.0.1:18092/?ct=http://127.0.0.1:18480,
# then in the console e.g. `await run('av_aac_51.mkv', {decode: 'aac51',
# seek: 35, seconds: 8})` (TS) or `{decode: 'hevc8,aac51'}` (fMP4);
# `await channels('av_aac_51.mkv', {decode: 'aac51'})` for the channels
# WebAudio gets.
#
#   run.sh [gotest|avsync|layout ...]   (default: all)
set -e
D=$(cd "$(dirname "$0")" && pwd)
PT=$D/../passthrough
export E2E_PREFIX=${E2E_PREFIX:-aud}
export E2E_WORK=${E2E_WORK:-$D/work}
export E2E_TOOLS_DIR=$D
export E2E_BASE_IMAGE=${E2E_BASE_IMAGE:-ghcr.io/webtor-io/content-transcoder:sha-079acfd}
export E2E_FFMPEG_IMAGE=$E2E_BASE_IMAGE
export E2E_NEW_IMAGE=${E2E_NEW_IMAGE:-aud-ct:local}
W=$E2E_WORK
P=$E2E_PREFIX
PORT=18480
mkdir -p "$W/img"
cd "$D"

arch=$(docker version --format '{{.Server.Arch}}')
(cd "$D/../.." && GOOS=linux GOARCH=$arch CGO_ENABLED=0 go build -ldflags '-w -s' -o "$W/img/server" . \
  && GOOS=linux GOARCH=$arch CGO_ENABLED=0 go test -c -o "$W/ct.test" ./services)
printf 'FROM %s\nCOPY server /app/server\n' "$E2E_BASE_IMAGE" > "$W/img/Dockerfile"
docker build -q -t "$E2E_NEW_IMAGE" "$W/img" >/dev/null
"$PT/ctl.sh" tools
"$PT/ctl.sh" media
docker exec "$P-tools" sh /e2e/gen.sh > "$W/gen.log" 2>&1

steps=${*:-gotest avsync layout}
fail=0
for step in $steps; do
  echo "== $step"
  case $step in
  gotest)
    docker rm -f "$P-gotest" >/dev/null 2>&1 || true
    docker run --rm --name "$P-gotest" -v "$W:/w" -e AUDIO_MEDIA=/w/media --entrypoint /w/ct.test "$E2E_BASE_IMAGE" \
      -test.run TestAudio_RealFFmpeg -test.v > "$W/gotest.log" 2>&1 || fail=1
    grep -E '^(--- |    --- )|RESULT' "$W/gotest.log" | sed 's/^ *audio_ffmpeg_test.go:[0-9]*: //'
    ;;
  avsync)
    "$PT/ctl.sh" up new-cap "$E2E_NEW_IMAGE" $PORT PASSTHROUGH_VIDEO_CODECS=hevc
    python3 avsync_audio.py "http://127.0.0.1:$PORT" "$W/avsync.json" || fail=1
    ;;
  layout)
    python3 layout.py "$W/layout.json" || fail=1
    ;;
  esac
done
[ $fail = 0 ] && echo "ALL PASS" || echo "SOMETHING FAILED"
exit $fail
