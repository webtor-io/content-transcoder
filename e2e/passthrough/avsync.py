"""A/V sync as a player that ignores edit lists (hls.js on MSE) sees it.

The source has a white frame and a click at every whole second. For each
route, from the start and after a seek, the served video and audio are
decoded as they are (raw timestamps: fMP4 tfdt+cto with -ignore_editlist,
TS PTS), flashes and click onsets are found, and each is mapped to movie
time with the playlist's SESSION-OFFSET. A click and a flash of the same
second should land at the same media time; the difference is the A/V error
(positive: audio late). The source's AAC has the usual 1024-sample
encoder delay: every route shows about 21 ms of it, as played without an
edit list.

Passthrough is held to limits (exit status 1 past them). The video: after
a seek within 5 ms of its movie time by the offset (the start_time 5 copy
too, which an absolute seek put 5 s off); from the start within 100 ms --
it shows 83 ms late there, the B-frame delay an edit-list-ignoring player
shows on every route. A/V: within 70 ms from the start (that delay against
the audio's priming, the same on the TS routes) and 50 ms after a seek (was
+162 ms before the audio was cut at the real start). The other routes are
measured, not judged.

Needs cte2e-new-cap (capability hevc) on 18080.

  python3 avsync.py <out.json>
"""
import json
import re
import subprocess
import sys

from lib import *  # noqa

CAP = "http://127.0.0.1:18080"
LAST = 69  # the last whole second of the 70 s sources


def tool(args):
    return subprocess.run(["docker", "exec", "cte2e-tools", *args], capture_output=True, text=True)


def flashes(path, mp4):
    pre = (["-ignore_editlist", "1"] if mp4 else []) + ["-copyts"]
    out = tool(["ffmpeg", "-nostdin", "-v", "error", *pre, "-i", wpath(path), "-an", "-vf",
                "signalstats,metadata=print:key=lavfi.signalstats.YAVG:file=-", "-f", "null", "-"]).stdout
    res, t = [], None
    for line in out.splitlines():
        m = re.search(r"pts_time:([-0-9.e]+)", line)
        if m:
            t = float(m.group(1))
        m = re.search(r"YAVG=([0-9.]+)", line)
        if m and t is not None and float(m.group(1)) > 128:
            res.append(round(t, 4))
    return res


def clicks(path, mp4):
    pre = (["-ignore_editlist", "1"] if mp4 else []) + ["-copyts"]
    err = tool(["ffmpeg", "-nostdin", "-v", "info", *pre, "-i", wpath(path), "-vn", "-af",
                "silencedetect=n=-30dB:d=0.005", "-f", "null", "-"]).stderr
    res = [round(float(x), 4) for x in re.findall(r"silence_end: ([-0-9.]+)", err)]
    # A click right at the start has no silence before it.
    first = tool(["ffprobe", "-v", "error", *pre[:-1], "-select_streams", "a:0", "-show_entries", "packet=pts_time", "-read_intervals", "%+#1", "-of", "csv=p=0", wpath(path)]).stdout.strip()
    return res, first


def save_track(base, sid, name, path):
    text, _ = wait_complete(base, sid, name)
    p = parse_media(text)
    with open(path, "wb") as f:
        if p["map"]:
            f.write(get(base, sid, p["map"], None)[2])
        for uri, _ in p["segments"]:
            f.write(get(base, sid, uri, None)[2])
    return p


def measure(label, src, decode, seek):
    odir = os.path.join(W, "fetch", "avsync", label)
    os.makedirs(odir, exist_ok=True)
    st, h, b, _ = post_session(CAP, src, decode)
    j = json.loads(b.decode())
    sid = j["id"]
    out = {"label": label, "route": [j["video_route"], j["route_reason"]], "runs": []}
    m = parse_master(get(CAP, sid, "index.m3u8")[2].decode())
    vname = strip_q(m["variants"][0]["URI"])
    aname = strip_q([x for x in m["media"] if x["TYPE"] == "AUDIO"][0]["URI"])
    for when in ("start", "seek"):
        if when == "seek":
            http("POST", f"{CAP}/session/{sid}/seek?t={seek}")
        mp4 = j["video_route"] == "passthrough"
        ext = "mp4" if mp4 else "ts"
        vp = save_track(CAP, sid, vname, os.path.join(odir, f"{when}_v.{ext}"))
        ap = save_track(CAP, sid, aname, os.path.join(odir, f"{when}_a.{ext}"))
        off = vp["offset"]
        fl = flashes(os.path.join(odir, f"{when}_v.{ext}"), mp4)
        cl, afirst = clicks(os.path.join(odir, f"{when}_a.{ext}"), mp4)
        # Every marker looks the same, so which second a marker is comes
        # from counting back from the last one (second 69 of a 70 s source).
        # The click list also has a silence_end at the end of the file; it
        # is off the clicks' sub-second phase and dropped.
        phase = sorted(c % 1 for c in cl)[len(cl) // 2]
        cl = [c for c in cl if abs((c - phase + 0.5) % 1 - 0.5) < 0.005]
        f0, c0 = LAST - (len(fl) - 1), LAST - (len(cl) - 1)
        pairs = []
        for sec in range(max(f0, c0), LAST + 1):
            f, c = fl[sec - f0], cl[sec - c0]
            pairs.append({"second": sec, "flash_media": f, "click_media": c,
                          "video_vs_offset_ms": round((f + off - sec) * 1000, 1),
                          "audio_vs_offset_ms": round((c + off - sec) * 1000, 1),
                          "av_ms": round((c - f) * 1000, 1)})
        vfirst = tool(["ffprobe", "-v", "error", *(["-ignore_editlist", "1"] if mp4 else []), "-select_streams", "v:0", "-show_entries", "packet=pts_time,dts_time", "-read_intervals", "%+#1", "-of", "csv=p=0", wpath(os.path.join(odir, f"{when}_v.{ext}"))]).stdout.strip()
        r = {"when": when, "offset": off, "audio_playlist_offset": ap["offset"], "first_video_second": f0, "first_audio_second": c0,
             "video_first_pts_dts": vfirst, "audio_first_pts": afirst,
             "video_vs_offset_ms": sorted({p["video_vs_offset_ms"] for p in pairs}),
             "audio_vs_offset_ms": sorted({p["audio_vs_offset_ms"] for p in pairs}),
             "av_ms": sorted({p["av_ms"] for p in pairs}), "pairs": pairs[:2]}
        out["runs"].append(r)
        print(label, json.dumps(r), flush=True)
    http("DELETE", f"{CAP}/session/{sid}")
    return out


# Limits for passthrough, ms: A/V from the start and after a seek, the
# video against its movie time from the start and after a seek.
AV_START_MS, AV_SEEK_MS, VIDEO_START_MS, VIDEO_SEEK_MS = 70, 50, 100, 5


def judge(r):
    """The failures of a passthrough measurement against the limits."""
    bad = []
    for run in r["runs"]:
        start = run["when"] == "start"
        lim = AV_START_MS if start else AV_SEEK_MS
        if any(abs(x) > lim for x in run["av_ms"]):
            bad.append(f"{r['label']} {run['when']}: A/V {run['av_ms']} ms past {lim}")
        vlim = VIDEO_START_MS if start else VIDEO_SEEK_MS
        if any(abs(x) > vlim for x in run["video_vs_offset_ms"]):
            bad.append(f"{r['label']} {run['when']}: video {run['video_vs_offset_ms']} ms off its movie time, past {vlim}")
    return bad


if __name__ == "__main__":
    res = [measure("passthrough_hevc", "avsync_hevc.mkv", "hevc8", 35),
           measure("passthrough_hevc_encoded_audio", "avsync_hevc_ac3.mkv", "hevc8", 35),
           measure("passthrough_hevc_start5", "avsync_hevc_st5.mkv", "hevc8", 35),
           measure("reencode_hevc", "avsync_hevc.mkv", None, 35),
           measure("copy_h264", "avsync_h264.mkv", None, 35)]
    json.dump(res, open(sys.argv[1], "w"), indent=1)
    failures = [f for r in res if r["label"].startswith("passthrough") for f in judge(r)]
    for f in failures:
        print("FAIL", f)
    print("avsync:", "FAIL" if failures else "PASS")
    sys.exit(1 if failures else 0)
