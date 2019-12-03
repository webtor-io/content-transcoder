# torrent-web-seeder

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
   --job-type value                          job type [$JOB_TYPE]
   --info-hash value                         info hash [$INFO_HASH]
   --file-path value                         file path [$FILE_PATH]
   --extra value                             extra [$EXTRA]
   --player                                  player
   --help, -h                                show help
   --version, -v                             print the version
```