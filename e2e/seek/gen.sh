#!/bin/sh
# Test sources for the seek e2e (old route: re-encode and copy), made with
# the transcoder image's own FFmpeg (jrottenberg/ffmpeg:8-alpine, 8.1.2) in
# the tools container, which mounts the work directory as /w.
set -e
mkdir -p /w/media
cd /w/media
F="ffmpeg -hide_banner -loglevel error -y"
X265="-c:v libx265 -preset ultrafast"
GOP10_265="keyint=240:min-keyint=240:scenecut=0:bframes=4:open-gop=1:log-level=error"

# Cues at known movie times: before the seek point (21, 26), one on screen
# across it (28-32), after it (33, 41, 61), and the last one (68).
cat > edge.srt <<'SRT'
1
00:00:01,000 --> 00:00:03,000
cue at 1

2
00:00:21,000 --> 00:00:23,000
cue at 21

3
00:00:26,000 --> 00:00:27,500
cue at 26

4
00:00:28,000 --> 00:00:32,000
straddle 28-32

5
00:00:33,000 --> 00:00:35,000
cue at 33

6
00:00:41,000 --> 00:00:43,000
cue at 41

7
00:01:01,000 --> 00:01:03,000
cue at 61

8
00:01:08,000 --> 00:01:09,000
last cue 68
SRT

# A/V markers: a white frame at every whole second (24 fps, frame
# n % 24 == 0) and a 10 ms 1 kHz click at every whole second, silence
# between. The gap7 variants leave out the markers of every second divisible
# by 7, so a player can tell which second it sees.
AVV="color=c=black:s=640x360:r=24,drawbox=c=white:t=fill:enable='eq(mod(n\,24)\,0)'"
AVA="aevalsrc='if(lt(mod(t\,1)\,0.01)\,0.8*sin(2*PI*1000*t)\,0)':s=48000:c=stereo"
G7V="color=c=black:s=640x360:r=24,drawbox=c=white:t=fill:enable='eq(mod(n\,24)\,0)*gt(mod(floor(n/24)\,7)\,0)'"
G7A="aevalsrc='if(lt(mod(t\,1)\,0.01)*gt(mod(floor(t)\,7)\,0)\,0.8*sin(2*PI*1000*t)\,0)':s=48000:c=stereo"

# Re-encode route (HEVC, 10 s GOPs, 4 B-frames): copied AAC, SRT; 70 s.
[ -f avs_hevc.mkv ] || $F -f lavfi -i "$AVV" -f lavfi -i "$AVA" -i edge.srt -t 70 -map 0:v -map 1:a -map 2:s \
  $X265 -x265-params "$GOP10_265" -c:a aac -b:a 128k -c:s srt avs_hevc.mkv
# The same with AC3 audio, which the route encodes (the control).
[ -f avs_hevc_ac3.mkv ] || $F -f lavfi -i "$AVV" -f lavfi -i "$AVA" -t 70 -map 0:v -map 1:a \
  $X265 -x265-params "$GOP10_265" -c:a ac3 -b:a 192k avs_hevc_ac3.mkv
# Its subtitles as ASS, as WebVTT (the route copies those), in MP4 as mov_text.
[ -f avs_hevc_ass.mkv ] || $F -i avs_hevc.mkv -map 0 -c copy -c:s ass avs_hevc_ass.mkv
[ -f avs_hevc_webvtt.mkv ] || $F -i avs_hevc.mkv -map 0 -c copy -c:s webvtt avs_hevc_webvtt.mkv
[ -f avs_hevc_movtext.mp4 ] || $F -i avs_hevc.mkv -map 0 -c:v copy -c:a copy -c:s mov_text -tag:v hvc1 avs_hevc_movtext.mp4
# Copy route (H.264, 10 s GOPs, 3 B-frames): the same markers and cues.
[ -f avs_h264.mkv ] || $F -f lavfi -i "$AVV" -f lavfi -i "$AVA" -i edge.srt -t 70 -map 0:v -map 1:a -map 2:s \
  -c:v libx264 -preset veryfast -g 240 -keyint_min 240 -sc_threshold 0 -bf 3 -c:a aac -b:a 128k -c:s srt avs_h264.mkv
# For the browser: markers with every 7th second missing, both routes.
[ -f avs_gap7_hevc.mkv ] || $F -f lavfi -i "$G7V" -f lavfi -i "$G7A" -i edge.srt -t 70 -map 0:v -map 1:a -map 2:s \
  $X265 -x265-params "$GOP10_265" -c:a aac -b:a 128k -c:s srt avs_gap7_hevc.mkv
[ -f avs_gap7_h264.mkv ] || $F -f lavfi -i "$G7V" -f lavfi -i "$G7A" -i edge.srt -t 70 -map 0:v -map 1:a -map 2:s \
  -c:v libx264 -preset veryfast -g 240 -keyint_min 240 -sc_threshold 0 -bf 3 -c:a aac -b:a 128k -c:s srt avs_gap7_h264.mkv

# Copy route, where FFmpeg's seek lands (24 fps, 95 s, B-frames unless
# said): keyframes every 10 s; the same without B-frames; keyframes only at
# 0 10 20 29.917 40 50 59.833 70 80 89.875 (inside FFmpeg's 3/23 s before
# 30 and 90, outside it before 60); the first remuxed to start at 5 s;
# keyframes only at 0 and 30; an MPEG-TS and an AVI.
V="testsrc2=size=640x360:rate=24"
H264="-c:v libx264 -preset veryfast -sc_threshold 0"
[ -f kf10_bf3.mkv ] || $F -f lavfi -i "$V" -f lavfi -i "$AVA" -i edge.srt -t 95 -map 0:v -map 1:a -map 2:s \
  $H264 -g 240 -keyint_min 240 -bf 3 -c:a aac -b:a 96k -c:s srt kf10_bf3.mkv
[ -f kf10_bf0.mkv ] || $F -f lavfi -i "$V" -f lavfi -i "$AVA" -t 95 -map 0:v -map 1:a \
  $H264 -g 240 -keyint_min 240 -bf 0 -c:a aac -b:a 96k kf10_bf0.mkv
[ -f kfwin.mkv ] || $F -f lavfi -i "$V" -f lavfi -i "$AVA" -t 95 -map 0:v -map 1:a $H264 -g 100000 -keyint_min 100000 -bf 3 \
  -force_key_frames "expr:eq(n,0)+eq(n,240)+eq(n,480)+eq(n,718)+eq(n,960)+eq(n,1200)+eq(n,1436)+eq(n,1680)+eq(n,1920)+eq(n,2157)" \
  -c:a aac -b:a 96k kfwin.mkv
[ -f kf10_bf3_st5.mkv ] || $F -i kf10_bf3.mkv -map 0 -c copy -output_ts_offset 5 kf10_bf3_st5.mkv
[ -f kf0_30.mkv ] || $F -f lavfi -i "$V" -f lavfi -i "$AVA" -t 95 -map 0:v -map 1:a $H264 -g 100000 -keyint_min 100000 -bf 3 \
  -force_key_frames "expr:eq(n,0)+eq(n,720)" -c:a aac -b:a 96k kf0_30.mkv
# The same with the cues: a copy seek to 30 lands on the file's first frame.
[ -f kf0_30_subs.mkv ] || $F -i kf0_30.mkv -i edge.srt -map 0 -map 1 -c copy -c:s srt kf0_30_subs.mkv
# MPEG-TS (no index), keyframes at 0 5 15 25 35 ...: FFmpeg's seek lands on
# the keyframe after the seek point (35.021 for 30: the video starts 21 ms
# into the file, after the AAC's priming).
[ -f kf5.ts ] || $F -f lavfi -i "$V" -f lavfi -i "$AVA" -t 95 -map 0:v -map 1:a $H264 -g 100000 -keyint_min 100000 -bf 3 \
  -force_key_frames "expr:eq(n,0)+eq(mod(n+120,240),0)" -c:a aac -b:a 96k kf5.ts
# AVI, time base 1001/24000, keyframes at 0 and 70.9 s: a seek to 60 reaches
# the first frame rounded to -1439 ticks (the probe's clamp).
[ -f avi_tick.avi ] || $F -f lavfi -i "testsrc2=size=640x360:rate=24000/1001" -t 95 $H264 -bf 0 -g 100000 -keyint_min 100000 \
  -force_key_frames "expr:eq(n,0)+eq(n,1700)" -an avi_tick.avi
ls -la /w/media
