"""Passthrough and route cases against a running transcoder.

  python3 cases.py <out.json> [case-name ...]

Needs the containers new-cap (capability hevc, port 18080) and new-off
(no capability, port 18180) from ctl.sh.
"""
import json
import os
import sys
import time

from lib import *  # noqa

CAP = "http://127.0.0.1:18080"   # new image, capability hevc
OFF = "http://127.0.0.1:18180"   # new image, capability empty

FULL = "hevc8,hevc10,hevc8-2160,hevc10-2160,hevc-high,hdr-pq"


class Case:
    def __init__(self, name):
        self.name = name
        self.checks = []
        self.ev = {}

    def check(self, what, ok, detail=""):
        self.checks.append({"check": what, "ok": bool(ok), "detail": detail})
        return ok

    def result(self):
        return {"case": self.name, "pass": all(c["ok"] for c in self.checks), "checks": self.checks, "evidence": self.ev}


def outdir(name):
    p = os.path.join(W, "fetch", name)
    os.makedirs(p, exist_ok=True)
    return p


def fetch_track(c, base, sid, pl_text, prefix, odir):
    """Init and every segment of a media playlist -> one file; checks the
    HTTP side and returns (init bytes, [segment bytes], path)."""
    p = parse_media(pl_text)
    c.check(f"{prefix}: EXT-X-MAP present, carries the query", p["map"] and p["map"].endswith("?" + Q), p["map"])
    st, h, init, _ = get(base, sid, p["map"], None)
    c.check(f"{prefix}: init 200 video/mp4 non-empty", st == 200 and h.get("Content-Type") == "video/mp4" and len(init) > 0,
            f"{st} {h.get('Content-Type')} {len(init)} B")
    segs = []
    bad = []
    for uri, _ in p["segments"]:
        st, h, b, _ = get(base, sid, uri, None)
        if st != 200 or h.get("Content-Type") != "video/mp4" or not b:
            bad.append((uri, st, h.get("Content-Type"), len(b)))
        segs.append(b)
    c.check(f"{prefix}: {len(segs)} segments 200 video/mp4", not bad and segs, str(bad[:3]))
    path = os.path.join(odir, prefix + ".mp4")
    with open(path, "wb") as f:
        f.write(init)
        for b in segs:
            f.write(b)
    return init, segs, path, p


def check_video_file(c, path, want, label):
    j = probe_track(path)
    st = (j.get("streams") or [{}])[0]
    c.ev[label + "_stream"] = {k: st.get(k) for k in ("codec_name", "codec_tag_string", "profile", "level", "width", "height", "pix_fmt", "color_transfer", "color_primaries")}
    c.check(f"{label}: ffprobe hevc/hvc1", st.get("codec_name") == "hevc" and st.get("codec_tag_string") == "hvc1", str(c.ev[label + "_stream"]))
    for k, v in want.items():
        c.check(f"{label}: {k} = {v}", st.get(k) == v, f"got {st.get(k)}")
    pk = j.get("packets", [])
    dts, bad = monotonic(pk)
    c.ev[label + "_packets"] = {"n": len(pk), "first_dts": dts[0] if dts else None, "last_dts": dts[-1] if dts else None,
                                "keyframes": sum(1 for p in pk if "K" in p.get("flags", ""))}
    c.check(f"{label}: {len(pk)} packets, DTS strictly increasing", pk and not bad, str(bad[:3]))
    return pk


def check_audio_file(c, path, label):
    j = probe_track(path)
    st = (j.get("streams") or [{}])[0]
    pk = j.get("packets", [])
    dts, bad = monotonic(pk)
    c.ev[label + "_stream"] = {k: st.get(k) for k in ("codec_name", "codec_tag_string", "profile", "channels", "sample_rate")}
    c.ev[label + "_packets"] = {"n": len(pk), "first_dts": dts[0] if dts else None, "last_dts": dts[-1] if dts else None}
    c.check(f"{label}: aac, 2 channels", st.get("codec_name") == "aac" and st.get("channels") == 2, str(c.ev[label + "_stream"]))
    c.check(f"{label}: {len(pk)} packets, DTS strictly increasing", pk and not bad, str(bad[:3]))


def cues_of(base, sid, pl_name):
    st, _, b, _ = get(base, sid, pl_name)
    p = parse_media(b.decode())
    cues = []
    for uri, _ in p["segments"]:
        _, _, vb, _ = get(base, sid, uri, None)
        cues += vtt_cues(vb.decode())
    return p, cues


def passthrough(name, src, decode, want, seek=None, base=CAP, subs=False, video_range="SDR"):
    c = Case(name)
    odir = outdir(name)
    st, h, b, dt = post_session(base, src, decode)
    body = b.decode()
    c.ev["post"] = {"status": st, "body": body, "secs": round(dt, 3)}
    try:
        resp = json.loads(body)
    except ValueError:
        c.check("POST 200 JSON", False, body)
        return c.result()
    c.check("POST 200, route passthrough/ok", st == 200 and resp.get("video_route") == "passthrough" and resp.get("route_reason") == "ok", body)
    sid = resp["id"]
    st, h, b, dt = get(base, sid, "index.m3u8")
    master = b.decode()
    c.ev["master"] = master
    c.ev["master_secs"] = round(dt, 3)
    m = parse_master(master)
    c.check("master 200", st == 200, str(st))
    c.check("master SESSION-OFFSET 0", "#EXT-X-SESSION-OFFSET:0.000" in master)
    v = m["variants"][0] if m["variants"] else {}
    codecs = v.get("CODECS", "")
    c.check(f"master RESOLUTION {want['width']}x{want['height']}", v.get("RESOLUTION") == f"{want['width']}x{want['height']}", v.get("RESOLUTION"))
    c.check(f"master VIDEO-RANGE {video_range}", v.get("VIDEO-RANGE") == video_range, v.get("VIDEO-RANGE"))
    c.check("master BANDWIDTH > 0", int(v.get("BANDWIDTH", "0")) > 0, v.get("BANDWIDTH"))
    vname = strip_q(v.get("URI", ""))
    # Like hls.js: the variant, then the init and segments as they come.
    t0 = time.time()
    st, _, b, _ = get(base, sid, vname)
    first = parse_media(b.decode())
    c.check("variant: VERSION 7, EVENT, SESSION-OFFSET 0", first["tags"].get("EXT-X-VERSION") == "7" and first["tags"].get("EXT-X-PLAYLIST-TYPE") == "EVENT" and first["offset"] == 0.0, str(first["tags"]))
    if first["segments"]:
        get(base, sid, first["segments"][0][0], None)
        c.ev["first_segment_secs"] = round(time.time() - t0, 3)
    text, waited = wait_complete(base, sid, vname)
    c.check("variant reaches ENDLIST", waited is not None, f"{waited}")
    init, segs, vpath, vp = fetch_track(c, base, sid, text, "video", odir)
    fourcc, hv = hvcc_of(init)
    mine = codec_string(fourcc, hv) if hv else None
    c.ev["codecs_master"] = codecs
    c.ev["codecs_from_init"] = mine
    c.check("CODECS in master == codec string of the served init's hvcC", codecs.split(",")[0] == mine, f"master {codecs} init {mine}")
    c.check("mp4a.40.2 in CODECS iff there is audio", ("mp4a.40.2" in codecs) == bool([x for x in m["media"] if x.get("TYPE") == "AUDIO"]), codecs)
    f = hvcc_fields(hv)
    c.check("no VPS/SPS/PPS in segment 0 samples", parameter_sets_in_samples(segs[0], f["length_size"]) == 0)
    check_video_file(c, vpath, want.get("probe", {}), "video")
    c.check(f"video duration ~ source ({want['duration']})", abs(vp["total"] - want["duration"]) < 0.6, f"playlist {vp['total']:.3f}")
    audio = [x for x in m["media"] if x.get("TYPE") == "AUDIO"]
    if audio:
        atext, _ = wait_complete(base, sid, strip_q(audio[0]["URI"]))
        ainit, asegs, apath, ap = fetch_track(c, base, sid, atext, "audio", odir)
        check_audio_file(c, apath, "audio")
        c.check("audio playlist SESSION-OFFSET 0", ap["offset"] == 0.0, str(ap["offset"]))
    if subs:
        sp, cues = cues_of(base, sid, "s0.m3u8")
        src_cues = source_cues(src)
        c.ev["cues_from_start"] = cues
        # The source's last cue is dropped at EOF by -fix_sub_duration (it
        # waits for a next cue that never comes) -- the old route's own
        # subtitle arguments; golden.py shows the old binary drops it too.
        c.ev["source_cues"] = src_cues
        c.check("subtitle cues from the start == source cues but the last", [round(x, 3) for x in cues] == [round(x, 3) for x in src_cues[:-1]], f"{cues} vs {src_cues}")
    if seek is not None:
        gen0 = vp["map"]
        st, _, b, dt = http("POST", f"{base}/session/{sid}/seek?t={seek}")
        sk = json.loads(b.decode()) if st == 200 else {}
        off = sk.get("offset")
        c.ev["seek"] = {"t": seek, "status": st, "answer": b.decode(), "secs": round(dt, 3)}
        c.check("seek 200 with offset", st == 200 and off is not None, b.decode())
        text, waited = wait_complete(base, sid, vname)
        st, _, gb, _ = http("GET", f"{base}/session/{sid}/seek")
        c.ev["seek_get"] = gb.decode()
        c.check("GET /seek offset == POST answer", json.loads(gb.decode()).get("offset") == off, gb.decode())
        init2, segs2, vpath2, vp2 = fetch_track(c, base, sid, text, "video_seek", odir)
        c.check("after seek: variant SESSION-OFFSET == seek offset", vp2["offset"] == off, f"{vp2['offset']} vs {off}")
        c.check("after seek: EXT-X-MAP names a new init (new generation)", vp2["map"] != gen0, f"{gen0} -> {vp2['map']}")
        st, _, mb, _ = get(base, sid, "index.m3u8")
        c.check("after seek: master SESSION-OFFSET == seek offset", f"#EXT-X-SESSION-OFFSET:{off:.3f}" in mb.decode(), mb.decode().splitlines()[1])
        dts0, cto0 = first_sample(init2, segs2[0])
        kfs = source_keyframes(src)
        shown = off + dts0 + cto0
        near = min(kfs, key=lambda k: abs(k - shown))
        c.ev["seek_first_sample"] = {"tfdt": dts0, "cto": round(cto0, 6), "shown_at_movie": round(shown, 4), "nearest_source_keyframe": near}
        c.check("after seek: first video DTS 0 (tfdt)", abs(dts0) < 0.0005, f"{dts0}")
        c.check("after seek: first video frame is a source keyframe at offset+pts", abs(shown - near) < 0.002 and near <= seek, f"shown {shown:.4f} keyframe {near}")
        c.check("after seek: video covers source end - keyframe", abs(vp2["total"] - (want["duration"] - near)) < 0.6, f"{vp2['total']:.3f} vs {want['duration'] - near:.3f}")
        check_video_file(c, vpath2, {}, "video_seek")
        if audio:
            atext, _ = wait_complete(base, sid, strip_q(audio[0]["URI"]))
            ainit2, asegs2, apath2, ap2 = fetch_track(c, base, sid, atext, "audio_seek", odir)
            check_audio_file(c, apath2, "audio_seek")
            c.check("after seek: audio SESSION-OFFSET == seek offset", ap2["offset"] == off, f"{ap2['offset']}")
            adts, _ = first_sample(ainit2, asegs2[0])
            c.ev["seek_audio_tfdt"] = adts
        if subs:
            sp2, cues2 = cues_of(base, sid, "s0.m3u8")
            src_cues = source_cues(src)
            want_cues = [round(x - off, 3) for x in src_cues[:-1] if x - off >= 0]
            c.ev["cues_after_seek"] = cues2
            c.ev["cues_after_seek_expected"] = want_cues
            c.check("after seek: subtitle playlist SESSION-OFFSET == seek offset", sp2["offset"] == off, f"{sp2['offset']}")
            c.check("after seek: cues == source cue - offset (but the last)", [round(x, 3) for x in cues2] == want_cues, f"{cues2} vs {want_cues}")
    http("DELETE", f"{base}/session/{sid}")
    return c.result()


def route(name, src, decode, want_status, want_route=None, want_reason=None, base=CAP, want_body=None):
    """POST only: the decision and how it is answered."""
    c = Case(name)
    st, h, b, dt = post_session(base, src, decode)
    body = b.decode()
    hdr = h.get("X-Video-Route-Reason")
    c.ev["post"] = {"status": st, "body": body, "X-Video-Route-Reason": hdr, "Retry-After": h.get("Retry-After"), "secs": round(dt, 3)}
    c.check(f"status {want_status}", st == want_status, f"{st} {body!r}")
    if st == 200:
        r = json.loads(body)
        c.check(f"route {want_route}/{want_reason}", r.get("video_route") == want_route and r.get("route_reason") == want_reason, body)
        http("DELETE", f"{base}/session/{r['id']}")
    else:
        c.check(f"X-Video-Route-Reason {want_reason}", hdr == want_reason, str(hdr))
        if want_body is not None:
            c.check(f"body {want_body!r}", body == want_body, repr(body))
    return c.result()


BODY415 = "resolution over 1080p is not supported\n"

CASES = {
    # -- passthrough, capability hevc --------------------------------------
    "pt_main8_1080": lambda: passthrough("pt_main8_1080", "main8_1080.mkv", "hevc8",
        {"width": 1920, "height": 1080, "duration": 70.021, "probe": {"profile": "1", "level": 120, "pix_fmt": "yuv420p"}},
        seek=35, subs=True),
    "pt_main10_1080": lambda: passthrough("pt_main10_1080", "main10_1080.mkv", "hevc10",
        {"width": 1920, "height": 1080, "duration": 40.0, "probe": {"profile": "2", "level": 120, "pix_fmt": "yuv420p10le"}},
        seek=35),
    "pt_main10_2160": lambda: passthrough("pt_main10_2160", "main10_2160.mkv", "hevc10,hevc10-2160",
        {"width": 3840, "height": 2160, "duration": 12.021, "probe": {"profile": "2", "level": 150, "pix_fmt": "yuv420p10le"}}),
    "pt_pq_1080": lambda: passthrough("pt_pq_1080", "pq_1080.mkv", "hevc10,hdr-pq",
        {"width": 1920, "height": 1080, "duration": 20.021, "probe": {"profile": "2", "color_transfer": "smpte2084"}},
        video_range="PQ"),
    "pt_pq_2160": lambda: passthrough("pt_pq_2160", "pq_2160.mkv", FULL,
        {"width": 3840, "height": 2160, "duration": 8.021, "probe": {"profile": "2", "color_transfer": "smpte2084"}},
        video_range="PQ"),
    # -- the route decision and its answers ----------------------------------
    "r_2160_nodecl_415": lambda: route("r_2160_nodecl_415", "main10_2160.mkv", None, 415, want_reason="no_declaration", want_body=BODY415),
    "r_2160_1080token_415": lambda: route("r_2160_1080token_415", "main10_2160.mkv", "hevc8,hevc10", 415, want_reason="needs_2160", want_body=BODY415),
    "r_2160_pending_415": lambda: route("r_2160_pending_415", "main10_2160.mkv", "unknown", 415, want_reason="declaration_pending", want_body=BODY415),
    "r_2160_garbage_415": lambda: route("r_2160_garbage_415", "main10_2160.mkv", "hvc1,HEVC10,x", 415, want_reason="no_declaration", want_body=BODY415),
    "r_2160_off_415": lambda: route("r_2160_off_415", "main10_2160.mkv", FULL, 415, want_reason="passthrough_off", base=OFF, want_body=BODY415),
    "r_pq2160_nopq_415": lambda: route("r_pq2160_nopq_415", "pq_2160.mkv", "hevc10,hevc10-2160", 415, want_reason="needs_pq", want_body=BODY415),
    "r_av1_2160_415": lambda: route("r_av1_2160_415", "av1_2160.mkv", FULL, 415, want_reason="not_hevc", want_body=BODY415),
    "r_av1_1080_reencode": lambda: route("r_av1_1080_reencode", "av1_1080.mkv", FULL, 200, "reencode", "not_hevc"),
    "r_dv5_1080_reencode": lambda: route("r_dv5_1080_reencode", "dv5_1080.mp4", FULL, 200, "reencode", "dv5"),
    "r_dv5_2160_415": lambda: route("r_dv5_2160_415", "dv5_2160.mp4", FULL, 415, want_reason="dv5", want_body=BODY415),
    "r_hevc10mp4_2160_pt": lambda: route("r_hevc10mp4_2160_pt", "hevc10_2160_plain.mp4", FULL, 200, "passthrough", "ok"),
    "r_h264_full_copy": lambda: route("r_h264_full_copy", "h264_1080.mkv", FULL, 200, "copy", "not_hevc"),
    "r_main10_8only": lambda: route("r_main10_8only", "main10_1080.mkv", "hevc8", 200, "reencode", "needs_main10"),
    "r_pq1080_nopq": lambda: route("r_pq1080_nopq", "pq_1080.mkv", "hevc10", 200, "reencode", "needs_pq"),
    "r_1080_pending": lambda: route("r_1080_pending", "main8_1080.mkv", "unknown", 200, "reencode", "declaration_pending"),
    "r_1080_nodecl": lambda: route("r_1080_nodecl", "main8_1080.mkv", None, 200, "reencode", "no_declaration"),
    "r_1080_off": lambda: route("r_1080_off", "main8_1080.mkv", FULL, 200, "reencode", "passthrough_off", base=OFF),
    # PQ in the HEVC VUI under an MKV Colour element that leaves the
    # transfer unspecified: ffprobe's stream level says nothing, the decoded
    # frame says PQ. Without hdr-pq the old route; with it passthrough,
    # labelled PQ.
    "r_pqvui_colourunspec": lambda: route("r_pqvui_colourunspec", "pqvui_colourunspec_1080.mkv", "hevc10", 200, "reencode", "needs_pq"),
    "pt_pqvui_colourunspec": lambda: passthrough("pt_pqvui_colourunspec", "pqvui_colourunspec_1080.mkv", "hevc10,hdr-pq",
        {"width": 1920, "height": 1080, "duration": 20.021, "probe": {"profile": "2"}},
        video_range="PQ"),
}

# Known defects: the case states the right behaviour and fails today. XFAIL
# does not fail the run; a pass (XPASS) does, so the entry is removed.
XFAIL = set()

if __name__ == "__main__":
    out = sys.argv[1]
    names = sys.argv[2:] or list(CASES)
    results = []
    failed = False
    for n in names:
        t0 = time.time()
        r = CASES[n]()
        r["secs"] = round(time.time() - t0, 1)
        results.append(r)
        fails = [x for x in r["checks"] if not x["ok"]]
        if n in XFAIL:
            verdict = "XPASS" if r["pass"] else "XFAIL"
            failed |= r["pass"]
        else:
            verdict = "PASS" if r["pass"] else "FAIL"
            failed |= not r["pass"]
        r["verdict"] = verdict
        print(f"{verdict} {n} ({r['secs']} s, {len(r['checks'])} checks)")
        for x in fails:
            print(f"    FAILED {x['check']}: {x['detail']}")
        sys.stdout.flush()
    json.dump(results, open(out, "w"), indent=1)
    sys.exit(1 if failed else 0)
