"""The old route's golden records (../passthrough/golden.py record) of the
production image and of an image with the seek fixes differ exactly where
the fixes mean them to, and nowhere else:

  copy seek (h264_1080.mkv)   the real-start probe is FFmpeg, not ffprobe
                              (that one call); the offset moves to where
                              FFmpeg's seek lands, in the seek answer and in
                              every playlist's SESSION-OFFSET; nothing else
                              of any playlist or segment
  re-encode seek (main8_1080) the run's call gains -ss 0 before the -map of
                              the copied AAC track and of the subtitle
                              output, nothing else; its audio and subtitle
                              playlists change (what they must be is
                              seek.py's business)

Those differences are checked, then put back to the production image's
values, and the rest goes through golden.py compare's own rules (calls,
answers, playlists, segment bytes but libx264's, the legacy GET, metric
families). A source a setup does not record is skipped.

  python3 golden_expect.py <old.json> <new.json> [--drop-source-probe]
"""
import json
import os
import re
import sys
import tempfile

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "passthrough"))
import golden  # noqa: E402

COPY, REENC = "h264_1080.mkv", "main8_1080.mkv"


def call_diff(ca, cb):
    return [i for i in range(max(len(ca), len(cb))) if (ca[i] if i < len(ca) else None) != (cb[i] if i < len(cb) else None)]


def expect_copy(a, b, bad):
    ra, rb = a["sources"][COPY], b["sources"][COPY]
    old_off = json.loads(ra["seek"][1])["offset"]
    new_off = json.loads(rb["seek"][1])["offset"]
    print(f"{COPY}: seek offset {old_off} -> {new_off}")
    if not (0 <= new_off <= old_off):
        bad.append(f"{COPY}: new offset {new_off} not at or before the old {old_off}")
    rb["seek"] = [rb["seek"][0], rb["seek"][1].replace(f'"offset":{new_off:.3f}', f'"offset":{old_off:.3f}')]
    back = lambda s: s.replace(f"#EXT-X-SESSION-OFFSET:{new_off:.3f}", f"#EXT-X-SESSION-OFFSET:{old_off:.3f}")
    for k in rb:
        if k.startswith("seek:"):
            v = rb[k]
            rb[k] = dict(v, playlist=back(v["playlist"])) if isinstance(v, dict) else [v[0], back(v[1])]
    ca, cb = a["calls"][COPY], b["calls"][COPY]
    d = call_diff(ca, cb)
    if len(ca) == len(cb) and len(d) == 1 and ca[d[0]][0] == "ffprobe" and "-read_intervals" in ca[d[0]] \
            and cb[d[0]][0] == "ffmpeg" and "framecrc" in cb[d[0]] and "-noaccurate_seek" in cb[d[0]]:
        print(f"{COPY} calls: only #{d[0]}, the real-start probe: {' '.join(cb[d[0]])}")
        cb[d[0]] = ca[d[0]]
    else:
        bad.append(f"{COPY} calls: differ in {d}, want only the real-start probe")


def expect_reencode(a, b, bad):
    ca, cb = a["calls"][REENC], b["calls"][REENC]
    d = call_diff(ca, cb)
    if len(ca) != len(cb) or len(d) != 1:
        bad.append(f"{REENC} calls: differ in {d}, want only the seek run's")
    else:
        x, y = ca[d[0]], cb[d[0]]
        z, cut = [], []
        for i, p in enumerate(x):
            if p == "-map" and i + 1 < len(x) and x[i + 1] in ("0:1", "0:2"):
                z += ["-ss", "0"]
                cut.append(x[i + 1])
            z.append(p)
        if y == z and "-ss" in x:
            print(f"{REENC} calls: only #{d[0]}, the seek run's, with -ss 0 before -map {' and '.join(cut)}")
            cb[d[0]] = x
        else:
            bad.append(f"{REENC} seek call: not the production one with -ss 0 before -map 0:1 and 0:2")
    ra, rb = a["sources"][REENC], b["sources"][REENC]
    for k in ("seek:a0.m3u8", "seek:s0.m3u8"):
        if ra.get(k) != rb.get(k):
            ta = sum(float(m) for m in re.findall(r"#EXTINF:([0-9.]+)", ra[k]["playlist"]))
            tb = sum(float(m) for m in re.findall(r"#EXTINF:([0-9.]+)", rb[k]["playlist"]))
            print(f"{REENC} {k}: changed, {len(ra[k]['segments'])} -> {len(rb[k]['segments'])} segments, {ta:.3f} -> {tb:.3f} s")
            rb[k] = ra[k]


def main(a_path, b_path, drop_source_probe):
    a, b = json.load(open(a_path)), json.load(open(b_path))
    bad = []
    if COPY in a["sources"] and COPY in b["sources"]:
        expect_copy(a, b, bad)
    if REENC in a["sources"] and REENC in b["sources"]:
        expect_reencode(a, b, bad)
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(b, f)
    try:
        if golden.compare(a_path, f.name, drop_source_probe):
            bad.append("golden.py compare: differences beyond the seek fixes (above)")
    finally:
        os.unlink(f.name)
    for x in bad:
        print("FAIL", x)
    print("golden_expect:", "FAIL" if bad else "PASS")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1], sys.argv[2], "--drop-source-probe" in sys.argv))
