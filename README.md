# content-transcoder

Transcodes HTTP-stream to HLS with additional features:
1. Web-access to transcoded content
2. On-demand transcoding
3. Quits after specific period of inactivity

## Requirements
1. FFmpeg 3+

## Basic usage
```
% ./server help
NAME:
   content-transcoder-server - runs content transcoder

USAGE:
   server [global options] command [command options] [arguments...]

VERSION:
   0.0.1

COMMANDS:
     help, h  Shows a list of commands or help for one command

GLOBAL OPTIONS:
   --host value, -H value                    listening host
   --port value, -P value                    listening port (default: 8080)
   --probe-port value, --pP value            probe port (default: 8081)
   --input value, -i value, --url value      input (url) [$INPUT, $ SOURCE_URL, $ URL]
   --output value, -o value                  output (local path) (default: "out")
   --content-prober-host value, --cpH value  hostname of the content prober service [$CONTENT_PROBER_SERVICE_HOST]
   --content-prober-port value, --cpP value  port of the content prober service (default: 50051) [$CONTENT_PROBER_SERVICE_PORT]
   --access-grace value, --ag value          access grace in seconds (default: 600) [$GRACE]
   --preset value                            transcode preset (default: "ultrafast") [$PRESET]
   --transcode-grace value, --tg value       transcode grace in seconds (default: 5) [$TRANSCODE_GRACE]
   --probe-timeout value, --pt value         probe timeout in seconds (default: 600) [$PROBE_TIMEOUT]
   --job-id value                            job id [$JOB_ID]
   --info-hash value                         info hash [$INFO_HASH]
   --file-path value                         file path [$FILE_PATH]
   --extra value                             extra [$EXTRA]
   --player                                  player
   --help, -h                                show help
   --version, -v                             print the version
```

## HEVC passthrough

A player that says what it decodes (`POST /session?...&decode=hevc10,hevc10-2160,hdr-pq`)
can get the source's HEVC as it is, in HLS fMP4, instead of the H.264 route
(see [docs/session-transcoding.md](docs/session-transcoding.md)). The transcoder
decides, and only for the codecs its capability lists:

```
--passthrough-video-codecs value       source video codecs handed to players as they are: hevc; empty (the default) passes none [$PASSTHROUGH_VIDEO_CODECS]
--passthrough-video-codecs-file value  file with the same list, re-read on every new session when it changes; overrides the flag [$PASSTHROUGH_VIDEO_CODECS_FILE]
```

With the capability empty, or a client that declares nothing, every session
takes the route it always had. `PASSTHROUGH_VIDEO_CODECS=hevc` works with
`DISABLE_VIDEO_TRANSCODING=true` too: passthrough copies the video, so the
sources a player can decode as they are play there as well.

`GET /capabilities` answers what a session opened now passes through,
`{"passthrough_video_codecs":["hevc"]}` or `[]` — for a service that must
not promise what the transcoder will not do (web-ui's Discover asks it).

## Requests for a session this pod does not have

A session is held in the memory of one pod. It is removed after 10 minutes
without a request, and every session on a pod is lost when the pod is
replaced. A request for such a session gets `404 session not found`. For a
GET or HEAD of a playlist, segment or init, that 404 comes only after a delay,
so that a player that asks again the moment it gets a 404 asks at most once
per delay per loader
(see [docs/session-transcoding.md](docs/session-transcoding.md), "Unknown session"):

```
--unknown-session-delay value  hold the 404 this long; 0 answers at once (default: 2s) [$UNKNOWN_SESSION_DELAY]
```

A proxy that measures the transcoder's time to first byte counts these 404s
at the delay. torrent-http-proxy files them under status class 400, so read
the transcoder's latency without that class (`status!="400"`): with it, a
p95 sits near the delay whenever abandoned tabs are open (on webtor.io, all
day).

## Example
```
cd server &&
rm -rf out/* && rm -rf tmp/* &&
go build -mod=vendor . &&
./server --input='https://github.com/Matroska-Org/matroska-test-files/raw/master/test_files/test5.mkv' --player=true
```
Then you can open your browser http://localhost:8080/player/ and watch movie
