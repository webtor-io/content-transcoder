"""Where each input channel ends up in the AAC 5.1 the transcoder encodes,
and at what level: FFmpeg's CLI with the transcoder's own output options
(-c:a libfdk_aac -ac 6 -b:a 384k), in the production image's FFmpeg.

For every channel of a 5.1(side) source (E-AC-3, as most multichannel
sources are) and of a 7.1 one (FLAC: the decoded layout a TrueHD 7.1 track
has too), a 2 s source with a tone in that channel alone (1 kHz; 60 Hz in
the LFE) is encoded that way, decoded, and the RMS level of every output
channel is read (astats). Expected from libswresample/rematrix.c (the -ac 6
layout is libfdk_aac's first 6-channel one, 5.1 = FL FR FC LFE BL BR,
fftools/ffmpeg_filter.c set_channel_layout):
  5.1(side): SL -> BL and SR -> BR at 1.0, every other channel to itself;
             no row sums over 1, so no normalization (0 dB each)
  7.1:       SL mixed into BL and SR into BR at 1/sqrt(2); the BL row sums
             to 1.707, and with S16 out (libfdk_aac takes S16) the matrix is
             normalized by it: every channel -4.6 dB, SL/SR -7.7 dB

  python3 layout.py <out.json>
"""
import json
import os
import re
import subprocess
import sys

PREFIX = os.environ.get("E2E_PREFIX", "aud")
LAYOUTS = {
    "5.1(side)": (["FL", "FR", "FC", "LFE", "SL", "SR"], ["-c:a", "eac3", "-b:a", "640k"], "mkv"),
    "7.1": (["FL", "FR", "FC", "LFE", "BL", "BR", "SL", "SR"], ["-c:a", "flac"], "mkv"),
}
OUT = ["FL", "FR", "FC", "LFE", "BL", "BR"]


def sh(cmd):
    return subprocess.run(["docker", "exec", PREFIX + "-tools", "sh", "-c", cmd], capture_output=True, text=True)


def levels(layout, names, codec, ext, ch):
    exprs = []
    for i, n in enumerate(names):
        f = 60 if n == "LFE" else 1000
        exprs.append(f"0.5*sin(2*PI*{f}*t)" if i == ch else "0")
    src = f"/tmp/layout_src.{ext}"
    enc = "/tmp/layout_out.m4a"
    r = sh(f"ffmpeg -hide_banner -loglevel error -y -f lavfi -i \"aevalsrc='{'|'.join(exprs)}':s=48000:c={layout}\" -t 2 {' '.join(codec)} {src}"
           f" && ffmpeg -hide_banner -loglevel error -y -i {src} -map 0:a -c:a libfdk_aac -ac 6 -b:a 384k {enc}"
           f" && ffprobe -v error -show_entries stream=channel_layout -of csv=p=0 {enc}"
           f" && ffmpeg -hide_banner -nostats -i {enc} -af astats -f null - 2>&1 | grep -E 'Channel:|RMS level dB'")
    if r.returncode != 0:
        raise SystemExit(f"{layout} {names[ch]}: {r.stderr}")
    lines = r.stdout.splitlines()
    out_layout = lines[0].strip()
    per, cur = {}, None
    for line in lines[1:]:
        m = re.search(r"Channel: (\d+)", line)
        if m:
            cur = int(m.group(1)) - 1
            continue
        m = re.search(r"RMS level dB: (\S+)", line)
        if m and cur is not None:
            v = m.group(1)
            per[OUT[cur]] = None if v in ("-inf", "inf") else round(float(v), 1)
            cur = None
    return out_layout, per


if __name__ == "__main__":
    res = {}
    for layout, (names, codec, ext) in LAYOUTS.items():
        # The reference: the same tone in the same channel, not remixed.
        res[layout] = {}
        for ch, n in enumerate(names):
            out_layout, per = levels(layout, names, codec, ext, ch)
            loud = {k: v for k, v in per.items() if v is not None and v > -60}
            res[layout][n] = {"output_layout": out_layout, "levels_db": per}
            print(f"{layout:10s} {n:4s} -> {out_layout:5s} " + " ".join(f"{k}={v}" for k, v in loud.items()))
    json.dump(res, open(sys.argv[1], "w"), indent=1)
