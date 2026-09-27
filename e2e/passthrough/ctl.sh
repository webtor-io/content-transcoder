#!/bin/sh
# Containers of the passthrough e2e, on the docker network cte2e.
#
#   ctl.sh tools                      FFmpeg image with the work dir as /w (sources, ffprobe)
#   ctl.sh media                      the source server (http://media:8000/<file>, and
#                                     /slow/<KB per second>/<file> throttled), logging every request
#   ctl.sh up <name> <image> <port> [ENV=VAL ...]
#                                     a transcoder cte2e-<name> on <port> (metrics on <port>+3),
#                                     fresh data dir; ffmpeg/ffprobe wrapped to record every call
#   ctl.sh down                       remove them all
set -e
D=$(cd "$(dirname "$0")" && pwd)
W=${E2E_WORK:-$D/work}
FFMPEG_IMAGE=${E2E_FFMPEG_IMAGE:-jrottenberg/ffmpeg:8-alpine}
mkdir -p "$W"
net() { docker network inspect cte2e >/dev/null 2>&1 || docker network create cte2e >/dev/null; }
case "$1" in
tools)
  net
  docker rm -f cte2e-tools >/dev/null 2>&1 || true
  docker run -d --name cte2e-tools --network cte2e -v "$W:/w" -v "$D:/e2e:ro" --entrypoint sleep "$FFMPEG_IMAGE" infinity >/dev/null
  ;;
media)
  net
  mkdir -p "$W/bin" "$W/srv" "$W/media"
  (cd "$D/mediasrv" && GOOS=linux GOARCH=$(docker version --format '{{.Server.Arch}}') CGO_ENABLED=0 go build -o "$W/bin/mediasrv" .)
  docker rm -f cte2e-media >/dev/null 2>&1 || true
  : > "$W/srv/access.log"
  docker run -d --name cte2e-media --network cte2e --network-alias media -v "$W/media:/media:ro" -v "$W/srv:/srv" -v "$W/bin:/bin2:ro" \
    --entrypoint /bin2/mediasrv "$FFMPEG_IMAGE" :8000 /media /srv/access.log >/dev/null
  ;;
up)
  net
  name=$2; image=$3; port=$4; shift 4
  envs=""
  for e in "$@"; do envs="$envs -e $e"; done
  docker rm -f "cte2e-$name" >/dev/null 2>&1 || true
  rm -rf "$W/runs/$name"; mkdir -p "$W/runs/$name/data" "$W/runs/$name/calls"
  # shellcheck disable=SC2086
  docker run -d --name "cte2e-$name" --network cte2e -p "127.0.0.1:$port:8080" -p "127.0.0.1:$((port+3)):8083" \
    -e OUTPUT=/data -e USE_PROM=true -e DEBUG=true $envs \
    -v "$W/runs/$name/data:/data" -v "$W/runs/$name/calls:/calls" \
    -v "$D/wrap/ffmpeg:/usr/local/bin/ffmpeg:ro" -v "$D/wrap/ffprobe:/usr/local/bin/ffprobe:ro" \
    "$image" ./server >/dev/null
  for i in $(seq 1 50); do curl -s -o /dev/null "http://127.0.0.1:$port/session" && break; sleep 0.2; done
  ;;
down)
  for c in $(docker ps -aq --filter name=cte2e-); do docker rm -f "$c" >/dev/null; done
  ;;
*)
  sed -n '2,12p' "$0"; exit 2
  ;;
esac
