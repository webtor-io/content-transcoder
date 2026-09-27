"""Seeks on the old route, measured on what a player gets.

Sources from gen.sh (a white frame and a click at every whole second, cues
at known movie times; H.264 files whose keyframes are where FFmpeg's seek
lands in known ways). For each case a session is opened, played from the
start and after a seek to 35 (a run at 30), and the served tracks are
decoded as they are (TS PTS, what hls.js places on its timeline; WebVTT
cue times, which hls.js places the same way without X-TIMESTAMP-MAP):

  A/V            click media time - flash media time of the same second
                 (positive: audio late); which second a marker is comes
                 from counting back from the last one (second 69)
  video/offset   flash media time + SESSION-OFFSET - its second, with the
                 media time as hls.js has it for TS: PTS minus the first
                 video PTS (Chrome 154, hls.js 1.6.14: the first frame plays
                 at 0.000); video_vs_offset_raw_ms without that
  cue/offset     cue start + SESSION-OFFSET - the cue's movie time, the
                 cue as served (what subtitle-translate stores)
  first frame    SESSION-OFFSET - the movie time of the run's first video
                 frame (found in the source by its decoded MD5; frame n is
                 n/24 s from the video's start, which is the video stream's
                 start_time minus the format's into the file: 21 ms in a TS
                 whose AAC has priming). error_vs_run0_ms is the same
                 without that start: what the run from the start plays the
                 frame at in hls.js, informational

Limits (exit status 1 past them), after the seek:
  every A/V case  audio and video playlists within 0.3 s of each other
  re-encode       A/V within 30 ms of the same session's A/V from the start
                  (a seek must not move the sound against the picture);
                  |A/V| <= 90 ms with copied AAC, 50 ms with encoded audio:
                  the re-encoded video's output is shifted by x264's
                  B-frame delay (its first PTS is 83 ms at 24 fps) and the
                  audio's is not, so the route plays the sound that much
                  early from the start too (-62 ms there: the copied AAC's
                  priming packet shifts the audio 21 ms the other way, and a
                  seek run has no priming packet); every cue within 0.1 s of
                  its movie time by the offset, none from before the seek
                  point, the cues at 33, 41 and 61 s there
  copy            |first frame| <= 5 ms; |video/offset| <= 5 ms;
                  |A/V| <= 50 ms; every cue within 1 ms of its movie time by
                  the offset, none from before the offset, and every one
                  after it but the file's last; a run that lands on the
                  file's first frame (offset 0) serves the run from the
                  start's subtitle playlist and segments
  from the start  |A/V| <= 70 ms and cues within 0.1 s, on both routes

  python3 seek.py <base url> <out.json> [case ...]
  python3 seek.py --judge <out.json>      judge a record again
"""
import json
import os
import re
import subprocess
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "passthrough"))
from lib import *  # noqa

LAST = 69  # the last whole second of the 70 s sources
SEEK = 35  # quantized to 30
RUN_AT = 30
AV_SEEK_MS, AV_SEEK_COPIED_AAC_MS, AV_SEEK_VS_START_MS, AV_START_MS = 50, 90, 30, 70
TOTAL_S, CUE_S, FIRST_MS, VIDEO_COPY_MS, CUE_COPY_S = 0.3, 0.1, 5, 5, 0.001
# The cues of edge.srt by movie second; the last one (68) is lost to
# -fix_sub_duration on every route and not asked for.
ALL_CUES = [1, 21, 26, 28, 33, 41, 61]


def tool(args):
    return subprocess.run(["docker", "exec", PREFIX + "-tools", *args], capture_output=True, text=True)


def flashes(path):
    out = tool(["ffmpeg", "-nostdin", "-v", "error", "-copyts", "-i", wpath(path), "-an", "-vf",
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


def clicks(path):
    err = tool(["ffmpeg", "-nostdin", "-v", "info", "-copyts", "-i", wpath(path), "-vn", "-af",
                "silencedetect=n=-30dB:d=0.005", "-f", "null", "-"]).stderr
    res = [round(float(x), 4) for x in re.findall(r"silence_end: ([-0-9.]+)", err)]
    if not res:
        return res
    # The end of the file is a silence_end too, off the clicks' phase.
    phase = sorted(c % 1 for c in res)[len(res) // 2]
    return [c for c in res if abs((c - phase + 0.5) % 1 - 0.5) < 0.005]


def first_pts(path, stream):
    out = tool(["ffprobe", "-v", "error", "-select_streams", stream, "-show_entries", "packet=pts_time",
                "-read_intervals", "%+#1", "-of", "csv=p=0", wpath(path)]).stdout.strip().strip(",")
    return float(out) if out else None


def save_track(base, sid, name, path):
    text, waited = wait_complete(base, sid, name, timeout=300)
    p = parse_media(text)
    with open(path, "wb") as f:
        for uri, _ in p["segments"]:
            f.write(get(base, sid, uri, None)[2])
    p["complete"] = waited is not None
    return p


def served_cues(base, sid, name):
    text, waited = wait_complete(base, sid, name, timeout=300)
    p = parse_media(text)
    cues = []
    for uri, _ in p["segments"]:
        vtt = get(base, sid, uri, None)[2].decode()
        for m in re.finditer(r"(?:(\d+):)?(\d\d):(\d\d)\.(\d\d\d) --> [^\n]*\n(.*)", vtt):
            t = int(m.group(1) or 0) * 3600 + int(m.group(2)) * 60 + int(m.group(3)) + int(m.group(4)) / 1000
            n = int(re.search(r"(\d+)", m.group(5)).group(1))
            cues.append({"start": t, "movie": n, "text": m.group(5)})
    return p, cues


def open_session(base, src):
    st, h, b, _ = post_session(base, src)
    if st != 200:
        raise RuntimeError(f"POST {src}: {st} {b[:200]}")
    j = json.loads(b.decode())
    m = parse_master(get(base, j["id"], "index.m3u8")[2].decode())
    names = {"video": strip_q(m["variants"][0]["URI"])}
    for x in m["media"]:
        names.setdefault(x["TYPE"].lower(), strip_q(x["URI"]))
    return j, names


def seek(base, sid, at=SEEK):
    st, _, b, _ = http("POST", f"{base}/session/{sid}/seek?t={at}&{Q}")
    return st, json.loads(b.decode()) if st == 200 else b.decode()


def fetch_dir(base, label):
    """Per image (port) as well: two images measured side by side must not
    read each other's files."""
    d = os.path.join(W, "fetch", label + "_" + base.rsplit(":", 1)[1])
    os.makedirs(d, exist_ok=True)
    return d


def av(base, label, src, subs=False):
    """A/V, video against the offset, cues against the offset; from the
    start and after the seek."""
    odir = fetch_dir(base, label)
    os.makedirs(odir, exist_ok=True)
    j, names = open_session(base, src)
    sid = j["id"]
    out = {"label": label, "source": src, "route": j.get("video_route", "?"), "runs": []}
    for when in ("start", "seek"):
        r = {"when": when}
        if when == "seek":
            r["seek_status"], r["seek_answer"] = seek(base, sid)
        vp = save_track(base, sid, names["video"], os.path.join(odir, f"{when}_v.ts"))
        ap = save_track(base, sid, names["audio"], os.path.join(odir, f"{when}_a.ts"))
        off = vp["offset"]
        fl, cl = flashes(os.path.join(odir, f"{when}_v.ts")), clicks(os.path.join(odir, f"{when}_a.ts"))
        f0, c0 = LAST - (len(fl) - 1), LAST - (len(cl) - 1)
        pairs = [{"second": s, "flash": fl[s - f0], "click": cl[s - c0]} for s in range(max(f0, c0), LAST + 1)]
        r.update({
            "offset": off, "audio_offset": ap["offset"], "complete": vp["complete"] and ap["complete"],
            "video_total": round(vp["total"], 3), "audio_total": round(ap["total"], 3),
            "first_flash_second": f0, "first_click_second": c0,
            "video_first_pts": first_pts(os.path.join(odir, f"{when}_v.ts"), "v:0"),
            "audio_first_pts": first_pts(os.path.join(odir, f"{when}_a.ts"), "a:0"),
            "av_ms": sorted({round((p["click"] - p["flash"]) * 1000, 1) for p in pairs}),
        })
        v0 = r["video_first_pts"] or 0
        r["video_vs_offset_ms"] = sorted({round((p["flash"] - v0 + off - p["second"]) * 1000, 1) for p in pairs})
        r["video_vs_offset_raw_ms"] = sorted({round((p["flash"] + off - p["second"]) * 1000, 1) for p in pairs})
        if subs:
            sp, cues = served_cues(base, sid, names["subtitles"])
            r["subtitle_offset"] = sp["offset"]
            r["subtitle_files"] = subtitle_files(base, sid, names["subtitles"])
            r["cues"] = [dict(c, vs_offset_s=round(c["start"] + sp["offset"] - c["movie"], 3)) for c in cues]
        out["runs"].append(r)
        print(label, when, json.dumps({k: v for k, v in r.items() if k != "cues"}), flush=True)
        if subs:
            print(label, when, "cues", [(c["movie"], c["start"], c["vs_offset_s"]) for c in r["cues"]], flush=True)
    http("DELETE", f"{base}/session/{sid}")
    return out


def subtitle_files(base, sid, name):
    """The subtitle playlist's segment lines and every segment's body."""
    text, _ = wait_complete(base, sid, name, timeout=300)
    p = parse_media(text)
    return [[strip_q(u), d] for u, d in p["segments"]] + [get(base, sid, u, None)[2].decode() for u, _ in p["segments"]]


_md5_index = {}
_video_start = {}


def video_start(src):
    """How far into the file the video starts: its start_time minus the
    format's."""
    if src not in _video_start:
        j = ffprobe(f"/w/media/{src}", "-select_streams", "v:0", "-show_entries", "format=start_time:stream=start_time")
        _video_start[src] = float(j["streams"][0]["start_time"]) - float(j["format"]["start_time"])
    return _video_start[src]


def md5_index(src):
    if src not in _md5_index:
        out = tool(["ffmpeg", "-nostdin", "-v", "error", "-i", f"/w/media/{src}", "-map", "0:v:0", "-f", "framemd5", "-"]).stdout
        idx = {}
        for i, l in enumerate(x for x in out.splitlines() if x and x[0] != "#"):
            idx.setdefault(l.split(",")[-1].strip(), i)
        _md5_index[src] = idx
    return _md5_index[src]


def first_frame(base, label, src, at):
    """The copy route's offset against the run's first video frame."""
    odir = fetch_dir(base, label)
    j, names = open_session(base, src)
    sid = j["id"]
    st, ans = seek(base, sid, at)
    t0 = time.time()
    p = None
    while time.time() - t0 < 120:
        s, _, b, _ = get(base, sid, names["video"])
        p = parse_media(b.decode()) if s == 200 else None
        if p and p["segments"]:
            break
        time.sleep(0.5)
    seg = os.path.join(odir, "v0.ts")
    with open(seg, "wb") as f:
        f.write(get(base, sid, p["segments"][0][0], None)[2])
    h = tool(["ffmpeg", "-nostdin", "-v", "error", "-i", wpath(seg), "-map", "0:v:0", "-frames:v", "1", "-f", "framemd5", "-"]).stdout
    h = [l for l in h.splitlines() if l and l[0] != "#"][0].split(",")[-1].strip()
    n = md5_index(src).get(h)
    pts = first_pts(seg, "v:0")
    http("DELETE", f"{base}/session/{sid}")
    movie = n / 24 + video_start(src) if n is not None else None
    r = {"label": label, "source": src, "seek": at, "seek_answer": ans, "offset": p["offset"],
         "first_frame_movie": movie, "first_frame_pts": pts,
         "error_ms": round((p["offset"] - movie) * 1000, 1) if movie is not None else None,
         "error_vs_run0_ms": round((p["offset"] - n / 24) * 1000, 1) if n is not None else None}
    print(label, json.dumps(r), flush=True)
    return r


def copy_cues(base, label, src, at=SEEK):
    """Copy route: the cues a seek run serves against the offset, and those
    of the run from the start."""
    j, names = open_session(base, src)
    sid = j["id"]
    r = {"label": label, "source": src, "route": j.get("video_route", "?"), "copy_cues": True}
    _, from0 = served_cues(base, sid, names["subtitles"])
    r["from0_files"] = subtitle_files(base, sid, names["subtitles"])
    r["from0_cues"] = [dict(c, vs_offset_s=round(c["start"] - c["movie"], 3)) for c in from0]
    r["seek_status"], r["seek_answer"] = seek(base, sid, at)
    sp, cues = served_cues(base, sid, names["subtitles"])
    r["offset"] = sp["offset"]
    r["files"] = subtitle_files(base, sid, names["subtitles"])
    r["cues"] = [dict(c, vs_offset_s=round(c["start"] + sp["offset"] - c["movie"], 3)) for c in cues]
    http("DELETE", f"{base}/session/{sid}")
    print(label, "offset", r["offset"], "cues", [(c["movie"], c["start"], c["vs_offset_s"]) for c in r["cues"]], flush=True)
    return r


CASES = {
    "reencode_av_subs": lambda b: av(b, "reencode_av_subs", "avs_hevc.mkv", subs=True),
    "reencode_av_encoded": lambda b: av(b, "reencode_av_encoded", "avs_hevc_ac3.mkv"),
    "reencode_subs_ass": lambda b: av(b, "reencode_subs_ass", "avs_hevc_ass.mkv", subs=True),
    "reencode_subs_webvtt": lambda b: av(b, "reencode_subs_webvtt", "avs_hevc_webvtt.mkv", subs=True),
    "reencode_subs_movtext": lambda b: av(b, "reencode_subs_movtext", "avs_hevc_movtext.mp4", subs=True),
    "copy_av": lambda b: av(b, "copy_av", "avs_h264.mkv", subs=True),
    "copy_kf10_bf3": lambda b: first_frame(b, "copy_kf10_bf3", "kf10_bf3.mkv", 35),
    "copy_kfwin_30": lambda b: first_frame(b, "copy_kfwin_30", "kfwin.mkv", 35),
    "copy_kfwin_60": lambda b: first_frame(b, "copy_kfwin_60", "kfwin.mkv", 65),
    "copy_kfwin_90": lambda b: first_frame(b, "copy_kfwin_90", "kfwin.mkv", 95),
    "copy_start5": lambda b: first_frame(b, "copy_start5", "kf10_bf3_st5.mkv", 35),
    "copy_no_bframes": lambda b: first_frame(b, "copy_no_bframes", "kf10_bf0.mkv", 35),
    "copy_first_keyframe": lambda b: first_frame(b, "copy_first_keyframe", "kf0_30.mkv", 35),
    # MPEG-TS: FFmpeg's seek lands on the keyframe after the seek point.
    "copy_ts_after_seek": lambda b: first_frame(b, "copy_ts_after_seek", "kf5.ts", 35),
    "copy_ts_after_seek_60": lambda b: first_frame(b, "copy_ts_after_seek_60", "kf5.ts", 65),
    # Embedded cues on the copy route: a run at 20, and one on the first frame.
    "copy_cues": lambda b: copy_cues(b, "copy_cues", "kf10_bf3.mkv"),
    "copy_cues_first_keyframe": lambda b: copy_cues(b, "copy_cues_first_keyframe", "kf0_30_subs.mkv"),
}
# Cues a seek to 35 must bring, by movie time; the last one of the file (68)
# is lost to -fix_sub_duration on every route and not asked for.
WANT_CUES_AFTER_SEEK = {33, 41, 61}


def judge_copy_cues(lab, when, cues, offset, bad):
    off = [(c["movie"], c["vs_offset_s"]) for c in cues if abs(c["vs_offset_s"]) > CUE_COPY_S]
    if off:
        bad.append(f"{lab} {when}: cues off their movie time by the offset (movie, error s): {off}")
    early = [c["movie"] for c in cues if c["movie"] < offset]
    if early:
        bad.append(f"{lab} {when}: cues from before the offset {offset} served: {early}")
    missing = {m for m in ALL_CUES if m >= offset} - {c["movie"] for c in cues}
    if missing:
        bad.append(f"{lab} {when}: cues missing: {sorted(missing)}")


def judge(r):
    bad = []
    lab = r["label"]
    if r.get("copy_cues"):
        judge_copy_cues(lab, "from the start", r["from0_cues"], 0, bad)
        judge_copy_cues(lab, "seek", r["cues"], r["offset"], bad)
        if r["offset"] == 0 and r["files"] != r["from0_files"]:
            bad.append(f"{lab} seek: offset 0, but the subtitle playlist and segments are not the run from the start's")
        return bad
    if "runs" in r:
        reenc = lab.startswith("reencode")
        start_av = r["runs"][0]["av_ms"]
        for run in r["runs"]:
            seek_ = run["when"] == "seek"
            if not run["complete"]:
                bad.append(f"{lab} {run['when']}: playlists did not complete")
            lim = AV_START_MS
            if seek_:
                lim = AV_SEEK_COPIED_AAC_MS if reenc and r["source"] != "avs_hevc_ac3.mkv" else AV_SEEK_MS
            if any(abs(x) > lim for x in run["av_ms"]) or not run["av_ms"]:
                bad.append(f"{lab} {run['when']}: A/V {run['av_ms']} ms past {lim}")
            if seek_ and reenc and start_av and any(abs(x - y) > AV_SEEK_VS_START_MS for x in run["av_ms"] for y in start_av):
                bad.append(f"{lab} seek: A/V {run['av_ms']} ms, from the start {start_av} ms: moved by the seek")
            if seek_ and abs(run["audio_total"] - run["video_total"]) > TOTAL_S:
                bad.append(f"{lab} seek: audio playlist {run['audio_total']} s against the video's {run['video_total']} s")
            if seek_ and not reenc and any(abs(x) > VIDEO_COPY_MS for x in run["video_vs_offset_ms"]):
                bad.append(f"{lab} seek: video {run['video_vs_offset_ms']} ms off its movie time by the offset")
            if "cues" in run and not reenc and seek_:
                judge_copy_cues(lab, "seek", run["cues"], run["offset"], bad)
            elif "cues" in run:
                off_cues = [(c["movie"], c["vs_offset_s"]) for c in run["cues"] if abs(c["vs_offset_s"]) > CUE_S]
                if off_cues:
                    bad.append(f"{lab} {run['when']}: cues off their movie time by the offset (movie, error s): {off_cues}")
                if seek_:
                    early = [c["movie"] for c in run["cues"] if c["movie"] < RUN_AT]
                    if early:
                        bad.append(f"{lab} seek: cues from before the seek point served: {early}")
                    missing = WANT_CUES_AFTER_SEEK - {c["movie"] for c in run["cues"]}
                    if missing:
                        bad.append(f"{lab} seek: cues missing: {sorted(missing)}")
    else:
        if r["error_ms"] is None or abs(r["error_ms"]) > FIRST_MS:
            bad.append(f"{lab}: offset {r['offset']}, the first frame is movie {r['first_frame_movie']}: {r['error_ms']} ms")
    return bad


if __name__ == "__main__":
    if sys.argv[1] == "--judge":
        res = json.load(open(sys.argv[2]))
    else:
        base, outp = sys.argv[1], sys.argv[2]
        names = sys.argv[3:] or list(CASES)
        res = [CASES[n](base) for n in names]
        json.dump(res, open(outp, "w"), indent=1)
    failures = [f for r in res for f in judge(r)]
    for f in failures:
        print("FAIL", f)
    print("seek:", "FAIL" if failures else "PASS", f"({len(failures)} failures)")
    sys.exit(1 if failures else 0)
