"""Pacing, real FFmpeg: a viewer that asks for segments like hls.js with a
30 s buffer (video by its own numbers, audio 4 s segments), and where the
run stops. Production is read from the run's own playlist on disk, the
process state from /proc in the container, source reads from the media
server's log.

  python3 pacing.py <src> <decode|-> <container> <port> <out.json>

<src> is served throttled as slow/<KB per second>/<file>: a local file
copies so fast that the whole run ends before the first pace poll.
"""
import glob
import json
import os
import subprocess
import sys
import time

from lib import *  # noqa


def ffmpeg_state(container):
    out = subprocess.run(["docker", "exec", container, "sh", "-c",
                          "for p in /proc/[0-9]*; do c=$(tr '\\0' ' ' < $p/cmdline 2>/dev/null); case \"$c\" in *hls_segment*|*segment_list*) "
                          "echo $(basename $p) $(awk '{print $3}' $p/stat);; esac; done"], capture_output=True, text=True).stdout.split()
    return out[1] if len(out) >= 2 else "-"


def source_bytes(src):
    n = 0
    for line in open(os.path.join(W, "srv", "access.log")):
        if f"/{src} " in line:
            n += int(line.split("sent=")[1].split()[0])
    return n


def produced(run_dir, name):
    try:
        return parse_media(open(os.path.join(run_dir, name + ".ffmpeg")).read())
    except FileNotFoundError:
        return None


def seg_start(p, n):
    t = 0.0
    for i, (_, d) in enumerate(p["segments"]):
        if i == n:
            return t
        t += d
    return t


def main(src, decode, container, port, out):
    base = f"http://127.0.0.1:{port}"
    st, h, b, _ = post_session(base, src, None if decode == "-" else decode)
    j = json.loads(b.decode())
    sid = j["id"]
    route = j.get("video_route")
    _, _, mb, _ = get(base, sid, "index.m3u8")
    m = parse_master(mb.decode())
    vname = strip_q(m["variants"][0]["URI"])
    aname = strip_q([x for x in m["media"] if x["TYPE"] == "AUDIO"][0]["URI"])
    runs = glob.glob(os.path.join(W, "runs", container.replace("cte2e-", ""), "data", "*", "runs", "*seek-0.000"))
    run_dir = max(runs, key=os.path.getmtime)
    series = []
    t0 = time.time()

    def ask(vn, an):
        """What hls.js asks for with the playhead at video segment vn and a
        30 s forward buffer: video vn, vn+1 (enough for 30 s of 10 s GOPs)
        and audio up to an."""
        vp = parse_media(get(base, sid, vname)[2].decode())
        ap = parse_media(get(base, sid, aname)[2].decode())
        vmax = amax = -1
        for n in (vn, vn + 1, vn + 2):
            if n < len(vp["segments"]):
                get(base, sid, vp["segments"][n][0], None)
                vmax = n
        for n in range(max(0, an - 7), an + 1):
            if n < len(ap["segments"]):
                get(base, sid, ap["segments"][n][0], None)
                amax = n
        return vmax, amax

    def settle(label, demand, max_wait=240):
        stable = 0
        last = None
        while time.time() - t0 < 10_000:
            p = produced(run_dir, vname)
            st = ffmpeg_state(container)
            prod = p["total"] if p else 0
            series.append({"t": round(time.time() - t0, 1), "phase": label, "produced": round(prod, 3),
                           "segments": len(p["segments"]) if p else 0, "state": st, "source_bytes": source_bytes(src)})
            get(base, sid, vname)  # a live player polls its playlist
            stable = stable + 1 if (prod == last and st == "T") else 0
            if p and p["endlist"]:
                return {"phase": label, "demand": demand, "produced": prod, "frozen": False, "ended": True}
            if stable >= 4:
                return {"phase": label, "demand": demand, "produced": prod, "lead": round(prod - demand, 3), "frozen": True,
                        "secs": round(time.time() - t0, 1)}
            last = prod
            if len(series) > max_wait:
                return {"phase": label, "demand": demand, "produced": prod, "frozen": False, "timeout": True}
            time.sleep(1)

    get(base, sid, "index.m3u8")
    results = []
    for label, vn, an in (("start", 0, 7), ("viewer at 3 min", 18, 52), ("viewer at 7.5 min", 45, 120)):
        vmax, amax = ask(vn, an)
        # Media demand as the transcoder defines it: the start of the
        # furthest segment asked for, per stream (from the run's playlists).
        vdem = seg_start(produced(run_dir, vname), vmax)
        adem = seg_start(produced(run_dir, aname), amax)
        demand = max(vdem, adem)
        r = settle(label, demand)
        r["video_seg"], r["audio_seg"], r["video_demand"], r["audio_demand"] = vmax, amax, round(vdem, 3), round(adem, 3)
        r["bytes_read"] = source_bytes(src)
        results.append(r)
        print(json.dumps(r), flush=True)
    http("DELETE", f"{base}/session/{sid}")
    json.dump({"src": src, "decode": decode, "route": route, "results": results, "series": series}, open(out, "w"), indent=1)
    if route == "passthrough":
        # Media-time pacing: frozen within the lead (5 min) plus what one
        # poll (1 s) of copying at the source's rate and one GOP add, or,
        # after a resume, not below the resume lead (4 min).
        ok = all(r.get("frozen") and 240 <= r["lead"] <= 330 for r in results)
        print("PASS" if ok else "FAIL", "passthrough pacing leads", [r.get("lead") for r in results], flush=True)
        if not ok:
            sys.exit(1)


if __name__ == "__main__":
    main(*sys.argv[1:])
