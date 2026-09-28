"""A/V after a seek for the audio each declaration gives, measured on the
served segments as a player that ignores edit lists (hls.js on MSE) places
them: ../passthrough/avsync.py's measurement (a white frame and a click at
every whole second, flashes and click onsets mapped to movie time by the
playlist's SESSION-OFFSET; positive A/V: audio late), on gen.sh's sources.

Limits (exit status 1 past them):
  passthrough      |A/V| <= 70 ms from the start, <= 50 ms after the seek
                   (avsync.py). From the start the error is the video's
                   B-frame delay (its first PTS, 83 ms; hls.js ignores the
                   edit list) against the audio's own delay (AAC priming:
                   43 ms for these libfdk_aac sources); E-AC-3 and AC-3 have
                   5.3 ms (256 samples), so a copied Dolby track is held to
                   85 ms there.
  re-encode        after the seek |A/V| <= 90 ms with copied AAC, <= 50 ms
                   with encoded audio (../seek/seek.py), and the audio within
                   50 ms of its movie time by the offset: the cut puts copied
                   AAC at its movie time, where the video is 83 ms late
                   (x264's B-frame delay), and the priming the run from the
                   start has is gone -- so seek.py's "within 30 ms of the
                   start's A/V" only holds for sources whose priming is
                   21 ms, and these have 43.
  copy             after the seek |A/V| <= 50 ms (../seek/seek.py)
  encoded, with a  within 5 ms of today's (the undeclared row of the same
  baseline row     source and route), from the start and after the seek:
                   the encode to 5.1 must not move the sound. DTS is held to
                   that alone: FFmpeg's DTS decoder delays it ~11 ms more
                   than E-AC-3, today (stereo) as with aac51, which puts it
                   past avsync.py's 50 ms after a passthrough seek.
The undeclared rows are today's audio, for comparison.

avsync.py counts markers back from the last one and drops the silence_end
of the file's end only when it is off the clicks' phase; an audio track that
ends on the phase (a whole number of seconds after the priming) gets one
click too many there and every pair one second off. The end of the file is
dropped here before counting.

  python3 avsync_audio.py <transcoder base url> <out.json> [case ...]
"""
import json
import os
import sys

D = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(D, "..", "passthrough"))
import avsync  # noqa: E402

_clicks = avsync.clicks


def clicks_without_the_end(path, mp4):
    """avsync.clicks without a silence_end at the file's end."""
    res, first = _clicks(path, mp4)
    pre = ["-ignore_editlist", "1"] if mp4 else []
    end = avsync.tool(["ffprobe", "-v", "error", *pre, "-show_entries", "format=duration", "-of", "csv=p=0", avsync.wpath(path)]).stdout.strip()
    try:
        if res and abs(res[-1] - float(end)) < 0.01:
            res = res[:-1]
    except ValueError:
        pass
    return res, first


avsync.clicks = clicks_without_the_end

CASES = [
    # label, source, decode, route, audio copied, baseline (today's row)
    ("passthrough_eac3_undeclared", "av_eac3_51.mkv", "hevc8", "passthrough", False, None),
    ("passthrough_dts_undeclared", "av_dts_51.mkv", "hevc8", "passthrough", False, None),
    ("passthrough_eac3_copied", "av_eac3_51.mkv", "hevc8,aac51,ac3,ec3", "passthrough", True, None),
    ("passthrough_ac3_copied", "av_ac3_51.mkv", "hevc8,aac51,ac3,ec3", "passthrough", True, None),
    ("passthrough_aac51_copied", "av_aac_51.mkv", "hevc8,aac51,ac3,ec3", "passthrough", True, None),
    ("passthrough_eac3_to_aac51", "av_eac3_51.mkv", "hevc8,aac51", "passthrough", False, "passthrough_eac3_undeclared"),
    ("passthrough_dts_to_aac51", "av_dts_51.mkv", "hevc8,aac51", "passthrough", False, "passthrough_dts_undeclared"),
    ("reencode_aac51_undeclared", "av_aac_51.mkv", None, "reencode", False, None),
    ("reencode_aac51_copied", "av_aac_51.mkv", "aac51", "reencode", True, None),
    ("reencode_eac3_to_aac51", "av_eac3_51.mkv", "aac51", "reencode", False, "reencode_aac51_undeclared"),
    ("reencode_flac71_to_aac51", "av_flac_71.mkv", "aac51", "reencode", False, "reencode_aac51_undeclared"),
    ("copy_aac51_undeclared", "av_h264_aac_51.mkv", None, "copy", False, None),
    ("copy_aac51_copied", "av_h264_aac_51.mkv", "aac51", "copy", True, None),
]


def judge(r, route, copied, base=None):
    runs = {x["when"]: x for x in r["runs"]}
    start, seek = runs["start"], runs["seek"]
    bad = []
    if base is not None:
        b = {x["when"]: x for x in base["runs"]}
        for when in ("start", "seek"):
            if any(abs(x - y) > 5 for x in runs[when]["av_ms"] for y in b[when]["av_ms"]):
                bad.append(f"{r['label']} {when}: A/V {runs[when]['av_ms']} ms, today's {b[when]['av_ms']}")
        if "dts" in r["label"]:
            return bad
    if route == "passthrough":
        if copied and r["label"].split("_")[1] in ("eac3", "ac3"):
            if any(abs(x) > 85 for x in start["av_ms"]):
                bad.append(f"{r['label']} start: A/V {start['av_ms']} ms past 85")
            if any(abs(x) > avsync.AV_SEEK_MS for x in seek["av_ms"]):
                bad.append(f"{r['label']} seek: A/V {seek['av_ms']} ms past {avsync.AV_SEEK_MS}")
            return bad
        return avsync.judge(r)
    lim = 90 if (route == "reencode" and copied) else 50
    if any(abs(x) > lim for x in seek["av_ms"]):
        bad.append(f"{r['label']} seek: A/V {seek['av_ms']} ms past {lim}")
    if route == "reencode" and any(abs(x) > 50 for x in seek["audio_vs_offset_ms"]):
        bad.append(f"{r['label']} seek: audio {seek['audio_vs_offset_ms']} ms off its movie time")
    return bad


if __name__ == "__main__":
    avsync.CAP = sys.argv[1]
    res, failures = [], []
    done = {}
    only = set(sys.argv[3:])
    for label, src, decode, route, copied, baseline in CASES:
        if only and label not in only:
            continue
        r = avsync.measure(label, src, decode, 35)
        if r["route"][0] != route:
            failures.append(f"{label}: route {r['route']}, want {route}")
        r["judged_as"] = [route, copied, baseline]
        res.append(r)
        done[label] = r
        if "dts" in label and baseline is None:
            continue  # today's DTS: the baseline, not judged
        failures += judge(r, route, copied, done.get(baseline))
    json.dump(res, open(sys.argv[2], "w"), indent=1)
    print("\n%-30s %-24s %-16s %-16s %-18s %-18s %s" % ("case", "route", "A/V start ms", "A/V seek ms", "audio/offset seek", "video/offset seek", "offset"))
    for r in res:
        runs = {x["when"]: x for x in r["runs"]}
        sk = runs["seek"]
        print("%-30s %-24s %-16s %-16s %-18s %-18s %s" % (r["label"], "/".join(r["route"]), runs["start"]["av_ms"], sk["av_ms"], sk["audio_vs_offset_ms"], sk["video_vs_offset_ms"], sk["offset"]))
    for f in failures:
        print("FAIL", f)
    print("avsync_audio:", "FAIL" if failures else "PASS")
    sys.exit(1 if failures else 0)
