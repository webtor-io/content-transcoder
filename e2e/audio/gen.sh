#!/bin/sh
# Test sources for the multichannel audio e2e, made with the transcoder
# image's own FFmpeg (8.1.2) in the tools container (../passthrough/ctl.sh
# tools, E2E_FFMPEG_IMAGE the production image), which mounts the work
# directory as /w.
#
# Every source is the avsync source of ../passthrough/gen.sh made small: a
# white frame at every whole second (24 fps, frame n % 24 == 0) and a 10 ms
# 1 kHz click at every whole second on every channel but the LFE, silence
# between; HEVC with 10 s GOPs and 4 B-frames (passthrough with hevc8, the
# re-encode route without); 70 s. Only the audio differs:
#
#   av_eac3_51.mkv    E-AC-3 5.1(side), 640 kb/s
#   av_ac3_51.mkv     AC-3 5.1(side), 448 kb/s
#   av_aac_51.mkv     AAC 5.1 (libfdk_aac, channel configuration 6), 384 kb/s
#   av_dts_51.mkv     DTS 5.1(side) (FFmpeg's experimental encoder)
#   av_truehd_51.mkv  TrueHD 5.1(side): FFmpeg's TrueHD encoder stops at 5.1
#                     (libavcodec/mlpenc.c), so no TrueHD 7.1 can be made here
#   av_flac_71.mkv    FLAC 7.1: the 7.1 -> 5.1 downmix a TrueHD 7.1 track
#                     takes too (the decoded layout is the same "7.1")
#   av_aac_71.mkv     AAC 7.1 (libfdk_aac)
#   av_aac_20.mkv     AAC stereo (the control)
#   av_aac_pce_51.mkv AAC 5.1(side) by FFmpeg's own encoder: a program config
#                     element (channel configuration 0; ffprobe: no layout),
#                     which Chrome does not play copied
#   av_h264_aac_51.mkv  H.264 (the copy route) with AAC 5.1
#
# craft.py (on the host, python3) makes two E-AC-3 copies movenc refuses
# from av_eac3_51.mkv: av_eac3_multi.mkv and av_eac3_corrupt.mkv.
set -e
mkdir -p /w/media
cd /w/media
F="ffmpeg -hide_banner -loglevel error -y"
X265="-c:v libx265 -preset ultrafast -x265-params keyint=240:min-keyint=240:scenecut=0:bframes=4:open-gop=1:log-level=error"
V="color=c=black:s=320x180:r=24,drawbox=c=white:t=fill:enable='eq(mod(n\,24)\,0)'"
CLICK="if(lt(mod(t\,1)\,0.01)\,0.8*sin(2*PI*1000*t)\,0)"
# One expression per channel, the LFE silent: 5.1 is FL FR FC LFE xL xR,
# 7.1 FL FR FC LFE BL BR SL SR.
A51="aevalsrc='$CLICK|$CLICK|$CLICK|0|$CLICK|$CLICK':s=48000:c=5.1(side)"
A71="aevalsrc='$CLICK|$CLICK|$CLICK|0|$CLICK|$CLICK|$CLICK|$CLICK':s=48000:c=7.1"
A20="aevalsrc='$CLICK|$CLICK':s=48000:c=stereo"

mk() { # name audio-source audio-options...
  name=$1; src=$2; shift 2
  [ -f "$name" ] || $F -f lavfi -i "$V" -f lavfi -i "$src" -t 70 -map 0:v -map 1:a $X265 "$@" "$name"
}
mk av_eac3_51.mkv "$A51" -c:a eac3 -b:a 640k
mk av_ac3_51.mkv "$A51" -c:a ac3 -b:a 448k
mk av_aac_51.mkv "$A51" -c:a libfdk_aac -b:a 384k
mk av_dts_51.mkv "$A51" -strict -2 -c:a dca
mk av_truehd_51.mkv "$A51" -strict -2 -c:a truehd
mk av_flac_71.mkv "$A71" -c:a flac
mk av_aac_71.mkv "$A71" -c:a libfdk_aac -b:a 512k
mk av_aac_20.mkv "$A20" -c:a aac -b:a 128k
mk av_aac_pce_51.mkv "$A51" -c:a aac -b:a 384k
[ -f av_h264_aac_51.mkv ] || $F -f lavfi -i "$V" -f lavfi -i "$A51" -t 70 -map 0:v -map 1:a \
  -c:v libx264 -preset veryfast -g 240 -keyint_min 240 -sc_threshold 0 -bf 3 -c:a libfdk_aac -b:a 384k av_h264_aac_51.mkv
for f in av_*.mkv; do
  printf '%s ' "$f"
  ffprobe -v error -select_streams a:0 -show_entries stream=codec_name,channels,channel_layout,bit_rate,profile -of csv=p=0 "$f"
done
