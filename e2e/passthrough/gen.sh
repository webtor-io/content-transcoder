#!/bin/sh
# Test sources for the passthrough e2e, made with the transcoder image's own
# FFmpeg (jrottenberg/ffmpeg:8-alpine, 8.1.2), in the tools container
# (ctl.sh tools), which mounts the work directory as /w.
set -e
mkdir -p /w/media
cd /w/media
F="ffmpeg -hide_banner -loglevel error -y"
X265="-c:v libx265 -preset ultrafast"
PQ="colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc:hdr10=1:master-display=G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,1):max-cll=1000,400"
# Tag the frames (FFmpeg 8 takes the encoder's colour properties from them;
# -color_trc on the output was dropped, and -colorspace alone wrote an MKV
# Colour element whose unspecified transfer hides the VUI's PQ from ffprobe).
PQTAGS="setparams=color_primaries=bt2020:color_trc=smpte2084:colorspace=bt2020nc"

cat > subs.srt <<'EOF'
1
00:00:01,000 --> 00:00:03,000
cue at 1

2
00:00:21,000 --> 00:00:23,000
cue at 21

3
00:00:26,000 --> 00:00:28,000
cue at 26

4
00:00:33,000 --> 00:00:35,000
cue at 33

5
00:00:41,000 --> 00:00:43,000
cue at 41

6
00:01:01,000 --> 00:01:03,000
cue at 61
EOF

# S1: Main 8-bit 1080p, 23.976 fps, 10 s GOP (240 frames, open GOP, 4 B-frames),
# AAC stereo (cut every 4 s by the route), SRT cues at 1/21/26/33/41/61 s; 70 s.
[ -f main8_1080.mkv ] || $F -f lavfi -i "testsrc2=size=1920x1080:rate=24000/1001" -f lavfi -i "sine=f=440:r=48000" -i subs.srt -t 70 \
  -map 0:v -map 1:a -map 2:s $X265 -x265-params "keyint=240:min-keyint=240:scenecut=0:bframes=4:open-gop=1:log-level=error" \
  -c:a aac -ac 2 -b:a 128k -c:s srt main8_1080.mkv

# S2: Main10 1080p, 25 fps, 2 s GOP, AC3 5.1 (encoded to AAC on every route); 40 s.
[ -f main10_1080.mkv ] || $F -f lavfi -i "testsrc2=size=1920x1080:rate=25,format=yuv420p10le" -f lavfi -i "sine=f=330:r=48000" -t 40 \
  -map 0:v -map 1:a $X265 -x265-params "keyint=50:min-keyint=50:scenecut=0:bframes=3:log-level=error" \
  -c:a ac3 -ac 6 -b:a 384k main10_1080.mkv

# S3: Main10 2160p, 24 fps, 2 s GOP, AAC; 12 s.
[ -f main10_2160.mkv ] || $F -f lavfi -i "testsrc2=size=3840x2160:rate=24,format=yuv420p10le" -f lavfi -i "sine=f=550:r=48000" -t 12 \
  -map 0:v -map 1:a $X265 -x265-params "keyint=48:min-keyint=48:scenecut=0:bframes=3:log-level=error" \
  -c:a aac -ac 2 main10_2160.mkv

# S4: Main10 1080p PQ (BT.2020, SMPTE 2084, mastering display, MaxCLL), 24 fps, 2 s GOP, AAC; 20 s.
[ -f pq_1080.mkv ] || $F -f lavfi -i "testsrc2=size=1920x1080:rate=24,format=yuv420p10le,$PQTAGS" -f lavfi -i "sine=f=660:r=48000" -t 20 \
  -map 0:v -map 1:a $X265 -x265-params "keyint=48:min-keyint=48:scenecut=0:bframes=3:$PQ:log-level=error" \
  -c:a aac -ac 2 pq_1080.mkv

# S5: the same PQ at 2160p; 8 s.
[ -f pq_2160.mkv ] || $F -f lavfi -i "testsrc2=size=3840x2160:rate=24,format=yuv420p10le,$PQTAGS" -f lavfi -i "sine=f=770:r=48000" -t 8 \
  -map 0:v -map 1:a $X265 -x265-params "keyint=48:min-keyint=48:scenecut=0:bframes=3:$PQ:log-level=error" \
  -c:a aac -ac 2 pq_2160.mkv

# S6: pacing source: Main 8-bit 720p, 24 fps, 10 s GOP, AAC; 15 min.
[ -f long_gop10.mkv ] || $F -f lavfi -i "testsrc2=size=1280x720:rate=24" -f lavfi -i "sine=f=440:r=48000" -t 900 \
  -map 0:v -map 1:a $X265 -crf 34 -x265-params "keyint=240:min-keyint=240:scenecut=0:bframes=4:log-level=error" \
  -c:a aac -ac 2 -b:a 96k long_gop10.mkv

# S7: H.264 1080p control, 24 fps, 2 s GOP, AAC, SRT; 40 s.
[ -f h264_1080.mkv ] || $F -f lavfi -i "testsrc2=size=1920x1080:rate=24" -f lavfi -i "sine=f=440:r=48000" -i subs.srt -t 40 \
  -map 0:v -map 1:a -map 2:s -c:v libx264 -preset veryfast -g 48 -keyint_min 48 -sc_threshold 0 \
  -c:a aac -ac 2 -c:s srt h264_1080.mkv

# S8/S9: AV1 1080p and 2160p (SVT-AV1), AAC.
[ -f av1_1080.mkv ] || $F -f lavfi -i "testsrc2=size=1920x1080:rate=24" -f lavfi -i "sine=f=440:r=48000" -t 6 \
  -map 0:v -map 1:a -c:v libsvtav1 -preset 12 -g 48 -c:a aac -ac 2 av1_1080.mkv
[ -f av1_2160.mkv ] || $F -f lavfi -i "testsrc2=size=3840x2160:rate=24" -f lavfi -i "sine=f=440:r=48000" -t 4 \
  -map 0:v -map 1:a -c:v libsvtav1 -preset 12 -g 48 -c:a aac -ac 2 av1_2160.mkv

# S10/S11: HEVC Main10 in MP4 (hvc1, moov at the end), to get a hand-made
# Dolby Vision profile 5 configuration record (dvcC) added by craft.py.
[ -f hevc10_1080_plain.mp4 ] || $F -f lavfi -i "testsrc2=size=1920x1080:rate=24,format=yuv420p10le" -f lavfi -i "sine=f=440:r=48000" -t 6 \
  -map 0:v -map 1:a $X265 -x265-params "keyint=48:min-keyint=48:scenecut=0:bframes=3:log-level=error" -tag:v hvc1 -c:a aac -ac 2 hevc10_1080_plain.mp4
[ -f hevc10_2160_plain.mp4 ] || $F -f lavfi -i "testsrc2=size=3840x2160:rate=24,format=yuv420p10le" -f lavfi -i "sine=f=440:r=48000" -t 4 \
  -map 0:v -map 1:a $X265 -x265-params "keyint=48:min-keyint=48:scenecut=0:bframes=3:log-level=error" -tag:v hvc1 -c:a aac -ac 2 hevc10_2160_plain.mp4

# A/V sync markers: a white frame at every whole second (24 fps, frame
# n % 24 == 0) and a 10 ms 1 kHz click at every whole second, silence
# between; 10 s GOPs with 4 B-frames (HEVC) / 3 (H.264); 70 s.
AVV="color=c=black:s=1280x720:r=24,drawbox=c=white:t=fill:enable='eq(mod(n\,24)\,0)'"
AVA="aevalsrc='if(lt(mod(t\,1)\,0.01)\,0.8*sin(2*PI*1000*t)\,0)':s=48000:c=stereo"
[ -f avsync_hevc.mkv ] || $F -f lavfi -i "$AVV" -f lavfi -i "$AVA" -t 70 -map 0:v -map 1:a \
  $X265 -x265-params "keyint=240:min-keyint=240:scenecut=0:bframes=4:open-gop=1:log-level=error" -c:a aac -b:a 128k avsync_hevc.mkv
[ -f avsync_h264.mkv ] || $F -f lavfi -i "$AVV" -f lavfi -i "$AVA" -t 70 -map 0:v -map 1:a \
  -c:v libx264 -preset veryfast -g 240 -keyint_min 240 -sc_threshold 0 -bf 3 -c:a aac -b:a 128k avsync_h264.mkv

# The same markers with AC3 audio, which every route encodes to AAC.
[ -f avsync_hevc_ac3.mkv ] || $F -f lavfi -i "$AVV" -f lavfi -i "$AVA" -t 70 -map 0:v -map 1:a \
  $X265 -x265-params "keyint=240:min-keyint=240:scenecut=0:bframes=4:open-gop=1:log-level=error" -c:a ac3 -b:a 192k avsync_hevc_ac3.mkv
# The HEVC one remuxed to start at 5 s (format start_time 5): a seek must
# land on the same movie time as on the original.
[ -f avsync_hevc_st5.mkv ] || $F -i avsync_hevc.mkv -map 0 -c copy -output_ts_offset 5 avsync_hevc_st5.mkv

# Pacing, old route: H.264 with the same 10 s GOPs, 15 min (copied).
[ -f long_h264_gop10.mkv ] || $F -f lavfi -i "testsrc2=size=1280x720:rate=24" -f lavfi -i "sine=f=440:r=48000" -t 900 \
  -map 0:v -map 1:a -c:v libx264 -preset ultrafast -crf 32 -g 240 -keyint_min 240 -sc_threshold 0 -c:a aac -ac 2 -b:a 96k long_h264_gop10.mkv

# PQ in the HEVC VUI, but an MKV Colour element without it: of these output
# colour options only the matrix reached the container (measured, frames
# untagged), and ffprobe's stream-level color_transfer, which the route
# reads, says unknown.
[ -f pqvui_colourunspec_1080.mkv ] || $F -f lavfi -i "testsrc2=size=1920x1080:rate=24,format=yuv420p10le" -f lavfi -i "sine=f=660:r=48000" -t 20 \
  -map 0:v -map 1:a $X265 -x265-params "keyint=48:min-keyint=48:scenecut=0:bframes=3:$PQ:log-level=error" \
  -color_primaries bt2020 -color_trc smpte2084 -colorspace bt2020nc -c:a aac -ac 2 pqvui_colourunspec_1080.mkv
ls -la /w/media
