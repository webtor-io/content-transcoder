"""Old route, real FFmpeg: the production image (1b25e28) against the new
image in the setups that must not change it. Every setup runs in a fresh
container (fresh /data, fresh FFmpeg call log), the same script of requests.

  python3 golden.py record <setup> <out.json>
  python3 golden.py compare <a.json> <b.json> [--drop-source-probe]

compare exits 1 when anything differs (FFmpeg/ffprobe calls, POST answers,
playlists, segment names/types/bytes -- libx264 bytes excepted, they differ
run to run on the same image -- the legacy GET, metric families).
"""
import hashlib
import json
import os
import re
import subprocess
import sys
import time

from lib import *  # noqa

# The image in production and the one under test.
OLD_IMAGE = os.environ.get("E2E_OLD_IMAGE", "ghcr.io/webtor-io/content-transcoder:sha-1b25e28")
NEW_IMAGE = os.environ.get("E2E_NEW_IMAGE", "ct-e2e:local")
FULL = "hevc8,hevc10,hevc8-2160,hevc10-2160,hevc-high,hdr-pq"

# setup -> (container name, image, port, env, decode per source or None)
SETUPS = {
    "old":              ("old", OLD_IMAGE, 18280, [], None),
    "old_decl":         ("old", OLD_IMAGE, 18280, [], FULL),
    "off_nodecl":       ("new-off", NEW_IMAGE, 18180, [], None),
    "off_full":         ("new-off", NEW_IMAGE, 18180, [], FULL),
    "cap_nodecl":       ("new-cap", NEW_IMAGE, 18080, ["PASSTHROUGH_VIDEO_CODECS=hevc"], None),
    "cap_unknown":      ("new-cap", NEW_IMAGE, 18080, ["PASSTHROUGH_VIDEO_CODECS=hevc"], "unknown"),
    "cap_garbage":      ("new-cap", NEW_IMAGE, 18080, ["PASSTHROUGH_VIDEO_CODECS=hevc"], "hvc1,HEVC10,x"),
    # Declarations the route turns down after its own look at the source
    # (needs_main10, needs_pq, dv5) and non-HEVC sources: old route too.
    # Negative control: passthrough on. The comparison must see it.
    "cap_full":         ("new-cap", NEW_IMAGE, 18080, ["PASSTHROUGH_VIDEO_CODECS=hevc"], FULL),
    "cap_short":        ("new-cap", NEW_IMAGE, 18080, ["PASSTHROUGH_VIDEO_CODECS=hevc"], "hevc8"),
}

SOURCES = ["h264_1080.mkv", "main8_1080.mkv", "main10_1080.mkv", "main10_2160.mkv",
           "pq_1080.mkv", "av1_1080.mkv", "av1_2160.mkv", "dv5_1080.mp4"]
# cap_short: only what the old route must still get with that declaration.
SHORT_SOURCES = ["h264_1080.mkv", "main10_1080.mkv", "pq_1080.mkv", "av1_1080.mkv", "av1_2160.mkv", "dv5_1080.mp4", "main10_2160.mkv"]

DURATIONS = {"h264_1080.mkv": 40.021, "main8_1080.mkv": 70.021, "main10_1080.mkv": 40.0}

ID = re.compile(r"[0-9a-f]{32}")


def norm(s):
    return ID.sub("<ID>", s)


def sha(b):
    return hashlib.sha256(b).hexdigest()[:16]


def play(base, sid, rec, key):
    """Master, every media playlist to its end, every segment's hash."""
    st, _, b, _ = get(base, sid, "index.m3u8")
    rec[key + "master"] = [st, norm(b.decode())]
    m = parse_master(b.decode())
    names = [strip_q(v["URI"]) for v in m["variants"]] + [strip_q(x["URI"]) for x in m["media"]]
    for n in names:
        text, waited = wait_complete(base, sid, n, timeout=400)
        p = parse_media(text)
        segs = []
        for uri, _ in p["segments"]:
            st, h, sb, _ = get(base, sid, uri, None)
            segs.append([strip_q(uri), st, h.get("Content-Type"), len(sb), sha(sb) if not strip_q(uri).endswith(".vtt") else sb.decode()])
        rec[key + n] = {"playlist": norm(text), "completed": waited is not None, "segments": segs}


def record(setup, out):
    cname, image, port, env, decode = SETUPS[setup]
    subprocess.run([os.path.join(D, "ctl.sh"), "up", cname, image, str(port), *env], check=True, capture_output=True)
    base = f"http://127.0.0.1:{port}"
    rec = {"setup": setup, "sources": {}}
    for src in (SHORT_SOURCES if setup == "cap_short" else SOURCES):
        r = {}
        st, h, b, _ = post_session(base, src, decode)
        body = b.decode()
        r["post"] = [st, h.get("Content-Type"), norm(body)]
        r["post_route_header"] = h.get("X-Video-Route-Reason")
        if st == 200:
            j = json.loads(body)
            r["post_fields"] = {k: j[k] for k in ("duration",)}
            r["route"] = [j.get("video_route"), j.get("route_reason")]
            sid = j["id"]
            play(base, sid, r, "")
            if DURATIONS.get(src, 0) > 35:
                st, _, sb, _ = http("POST", f"{base}/session/{sid}/seek?t=35")
                r["seek"] = [st, sb.decode()]
                play(base, sid, r, "seek:")
            http("DELETE", f"{base}/session/{sid}")
        # source_url last: the handler takes the rest of the raw query as the URL.
        st, h, b, _ = http("GET", f"{base}/index.m3u8?{Q}&source_url=http%3A%2F%2Fmedia%3A8000%2F{src}")
        r["legacy"] = [st, h.get("Content-Type"), norm(b.decode())]
        rec["sources"][src] = r
        print(setup, src, r["post"][0], r.get("route"), flush=True)
    # FFmpeg and ffprobe calls, in order, per source.
    calls_dir = os.path.join(W, "runs", cname, "calls")
    time.sleep(1)
    calls = {}
    for f in sorted(os.listdir(calls_dir), key=lambda n: float(n.split("-")[0])):
        args = open(os.path.join(calls_dir, f)).read().splitlines()
        tool = args[0][1:]
        srcs = [a for a in args if "media:8000/" in a]
        src = srcs[0].rsplit("/", 1)[1] if srcs else "?"
        calls.setdefault(src, []).append([tool] + args[1:])
    rec["calls"] = calls
    st, _, b, _ = http("GET", f"http://127.0.0.1:{port + 3}/metrics")
    rec["metrics"] = b.decode()
    json.dump(rec, open(out, "w"), indent=1)


def metric_families(text):
    fam = {}
    for line in text.splitlines():
        if line.startswith("# HELP "):
            n, _, h = line[7:].partition(" ")
            fam.setdefault(n, {})["help"] = h
        elif line.startswith("# TYPE "):
            n, _, t = line[7:].partition(" ")
            fam.setdefault(n, {})["type"] = t
    labels = {}
    for line in text.splitlines():
        if line.startswith("#") or not line.strip():
            continue
        m = re.match(r"([a-zA-Z_:][a-zA-Z0-9_:]*)(\{([^}]*)\})?", line)
        name = m.group(1)
        ls = tuple(sorted(re.findall(r'([a-zA-Z_][a-zA-Z0-9_]*)="', m.group(3) or "")))
        labels.setdefault(name, set()).add(ls)
    return fam, labels


def is_source_probe(call):
    """The passthrough route's own look at an HEVC source (CT-2)."""
    return call[0] == "ffprobe" and "-show_data" in call


def compare(a_path, b_path, drop_source_probe=False):
    a, b = json.load(open(a_path)), json.load(open(b_path))
    dropped = 0
    if drop_source_probe:
        for src, calls in b["calls"].items():
            kept = [c for c in calls if not is_source_probe(c)]
            dropped += len(calls) - len(kept)
            b["calls"][src] = kept
    diffs = []
    noise = []
    same = 0
    for src, ra in a["sources"].items():
        rb = b["sources"].get(src)
        if rb is None:
            continue
        for k in sorted(set(ra) | set(rb)):
            if k in ("route", "post_route_header"):
                continue  # new fields/headers, reported separately
            va, vb = ra.get(k), rb.get(k)
            if k == "post":
                # The new binary adds video_route/route_reason to the JSON.
                if va and vb and va[0] == 200 == vb[0]:
                    ja, jb = json.loads(va[2]), json.loads(vb[2])
                    va = [va[0], va[1], {x: ja[x] for x in ja}]
                    vb = [vb[0], vb[1], {x: jb[x] for x in ja}]
            if va == vb:
                same += 1
            elif isinstance(va, dict) and isinstance(vb, dict) and va.get("playlist") == vb.get("playlist") and \
                    [x[:3] for x in va["segments"]] == [x[:3] for x in vb["segments"]] and \
                    ra.get("route", [None])[0] in (None, "reencode") and rb.get("route", [None])[0] in (None, "reencode") and \
                    any("-c:v" in c and c[c.index("-c:v") + 1] == "h264" for c in a["calls"].get(src, [])):
                # Same playlist, same segment names/status/type; the bytes of
                # a libx264 re-encode differ run to run (old vs old shows it).
                noise.append((src, k, [(x[0], x[3]) for x in va["segments"] if x[4] not in [y[4] for y in vb["segments"]]],
                              [(x[0], x[3]) for x in vb["segments"] if x[4] not in [y[4] for y in va["segments"]]]))
                same += 1
            else:
                diffs.append((src, k, va, vb))
        ca, cb = a["calls"].get(src, []), b["calls"].get(src, [])
        if ca == cb:
            same += 1
        else:
            diffs.append((src, "calls", ca, cb))
    print(f"{a['setup']} vs {b['setup']}: {same} items equal, {len(diffs)} differ; FFmpeg/ffprobe calls equal for {sum(1 for s in a['sources'] if s in b['sources'] and a['calls'].get(s) == b['calls'].get(s))}/{sum(1 for s in a['sources'] if s in b['sources'])} sources")
    for src, k, x, y in noise:
        print(f"  (re-encode bytes differ, playlist equal) {src} {k}: {x} vs {y}")
    for src, k, va, vb in diffs:
        print(f"  DIFF {src} {k}")
        if k == "calls":
            print(f"    {a['setup']}: {len(va)} calls {[c[0] for c in va]}")
            print(f"    {b['setup']}: {len(vb)} calls {[c[0] for c in vb]}")
            for i in range(max(len(va), len(vb))):
                x = va[i] if i < len(va) else None
                y = vb[i] if i < len(vb) else None
                if x != y:
                    print(f"      #{i} {a['setup']}: {' '.join(x) if x else None}")
                    print(f"      #{i} {b['setup']}: {' '.join(y) if y else None}")
        else:
            print(f"    {a['setup']}: {json.dumps(va)[:600]}")
            print(f"    {b['setup']}: {json.dumps(vb)[:600]}")
    fa, la = metric_families(a["metrics"])
    fb, lb = metric_families(b["metrics"])
    missing = [n for n in fa if n not in fb]
    changed = [n for n in fa if n in fb and fa[n] != fb[n]]
    lost_labels = [n for n in la if n in lb and not la[n] <= lb[n]]
    print(f"  metrics: {len(fa)} families before, {len(fb)} after; missing {missing}; help/type changed {changed}; label sets lost {lost_labels}")
    print(f"  new families: {sorted(n for n in fb if n not in fa)}")
    if drop_source_probe:
        print(f"  source probes left out of the comparison: {dropped}")
    return diffs or missing or changed or lost_labels


if __name__ == "__main__":
    if sys.argv[1] == "record":
        record(sys.argv[2], sys.argv[3])
    else:
        sys.exit(1 if compare(sys.argv[2], sys.argv[3], "--drop-source-probe" in sys.argv) else 0)
