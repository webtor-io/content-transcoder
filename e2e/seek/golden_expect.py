"""The old route's golden records (../passthrough/golden.py record) of the
production image and of an image with the seek fixes differ exactly where
the fixes mean them to, and nowhere else:

  copy seek (h264_1080.mkv)   the real-start probe is FFmpeg, not ffprobe
                              (that one call); the offset moves to where
                              FFmpeg's seek lands, in the seek answer and in
                              every playlist's SESSION-OFFSET; the seek run's
                              call gains -itsoffset <30 - offset> before -i
                              and -ss 0 before the -map of the subtitle
                              output, nothing else; its subtitle playlist
                              changes (seek.py's business), no other
                              playlist or segment does
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
QUANTIZED = 30.0  # golden.py seeks to 35


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
    probe = [i for i in d if ca[i][0] == "ffprobe" and "-read_intervals" in ca[i]
             and cb[i][0] == "ffmpeg" and "framecrc" in cb[i] and "-noaccurate_seek" in cb[i]]
    runs = [i for i in d if i not in probe]
    if len(ca) == len(cb) and len(probe) == 1 and len(runs) == 1:
        print(f"{COPY} calls: #{probe[0]}, the real-start probe: {' '.join(cb[probe[0]])}")
        cb[probe[0]] = ca[probe[0]]
        x, y = ca[runs[0]], cb[runs[0]]
        # The value as the run wrote it (6 decimals of the probe's answer),
        # within a millisecond of what the 3-decimal offset says.
        its = y[y.index("-itsoffset") + 1] if "-itsoffset" in y else None
        if (its is None) != (QUANTIZED - new_off <= 0) or its is not None and abs(float(its) - (QUANTIZED - new_off)) > 0.001:
            bad.append(f"{COPY} seek call: -itsoffset {its}, the offset {new_off} says {QUANTIZED - new_off:.3f}")
        z = []
        for i, p in enumerate(x):
            if p == "-i" and i > 0 and x[i - 1] == "-noaccurate_seek" and its is not None:
                z += ["-itsoffset", its]
            if p == "-map" and i + 1 < len(x) and x[i + 1] == "0:2":
                z += ["-ss", "0"]
            z.append(p)
        if y == z:
            print(f"{COPY} calls: #{runs[0]}, the seek run's, with -itsoffset {its} and -ss 0 before -map 0:2 (the subtitles)")
            cb[runs[0]] = x
        else:
            bad.append(f"{COPY} seek call: not the production one with -itsoffset {its} and -ss 0 before -map 0:2")
    else:
        bad.append(f"{COPY} calls: differ in {d}, want only the real-start probe and the seek run")
    k = "seek:s0.m3u8"
    if ra.get(k) != rb.get(k):
        print(f"{COPY} {k}: changed, cues {[l for s in ra[k]['segments'] for l in s[4].splitlines() if '-->' in l][:4]}"
              f" -> {[l for s in rb[k]['segments'] for l in s[4].splitlines() if '-->' in l][:4]} ...")
        rb[k] = ra[k]


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
