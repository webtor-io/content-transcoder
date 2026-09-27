"""Client side of the passthrough e2e: plays sessions of a running
content-transcoder like a player would, and checks what comes back with
FFmpeg's own ffprobe (in the cte2e-tools container, same build as the
transcoder image)."""
import json
import os
import re
import struct
import subprocess
import time
import urllib.error
import urllib.request

D = os.path.dirname(os.path.abspath(__file__))
# Everything the run makes (sources, container data, fetched segments,
# records); mounted as /w in the tools container.
W = os.environ.get("E2E_WORK", os.path.join(D, "work"))
Q = "token=T&api-key=K"
# Container names: <prefix>-tools etc. (ctl.sh, E2E_PREFIX).
PREFIX = os.environ.get("E2E_PREFIX", "cte2e")


def http(method, url, headers=None, timeout=330):
    req = urllib.request.Request(url, method=method, headers=headers or {})
    t0 = time.time()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, dict(r.headers), r.read(), time.time() - t0
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers), e.read(), time.time() - t0


def post_session(base, src, decode=None, extra=Q):
    q = extra
    if decode is not None:
        q = "decode=" + decode + ("&" + extra if extra else "")
    return http("POST", f"{base}/session?{q}", {"X-Source-Url": f"http://media:8000/{src}"})


def get(base, sid, name, query=Q):
    url = f"{base}/session/{sid}/{name}"
    if query and "?" not in name:
        url += "?" + query
    return http("GET", url)


def strip_q(uri):
    return uri.split("?", 1)[0]


# --- playlists ---------------------------------------------------------------

def parse_attrs(s):
    out = {}
    for m in re.finditer(r'([A-Z0-9-]+)=("([^"]*)"|[^,]*)', s):
        out[m.group(1)] = m.group(3) if m.group(3) is not None else m.group(2)
    return out


def parse_master(text):
    m = {"media": [], "variants": [], "tags": []}
    lines = text.splitlines()
    for i, line in enumerate(lines):
        if line.startswith("#EXT-X-MEDIA:"):
            m["media"].append(parse_attrs(line[len("#EXT-X-MEDIA:"):]))
        elif line.startswith("#EXT-X-STREAM-INF:"):
            a = parse_attrs(line[len("#EXT-X-STREAM-INF:"):])
            a["URI"] = lines[i + 1]
            m["variants"].append(a)
        elif line.startswith("#"):
            m["tags"].append(line)
    return m


def parse_media(text):
    p = {"segments": [], "map": None, "offset": None, "endlist": "#EXT-X-ENDLIST" in text, "tags": {}}
    dur = None
    for line in text.splitlines():
        if line.startswith("#EXT-X-MAP:"):
            p["map"] = parse_attrs(line[len("#EXT-X-MAP:"):]).get("URI")
        elif line.startswith("#EXT-X-SESSION-OFFSET:"):
            p["offset"] = float(line.split(":", 1)[1])
        elif line.startswith("#EXTINF:"):
            dur = float(line[len("#EXTINF:"):].split(",")[0])
        elif line.startswith("#EXT-X-"):
            k, _, v = line[1:].partition(":")
            p["tags"][k] = v
        elif line and not line.startswith("#") and dur is not None:
            p["segments"].append((line, dur))
            dur = None
    p["total"] = sum(d for _, d in p["segments"])
    return p


def wait_complete(base, sid, name, timeout=300, poll=1.0):
    """Polls a media playlist like a live player until #EXT-X-ENDLIST."""
    t0 = time.time()
    while True:
        st, h, b, _ = get(base, sid, name)
        text = b.decode()
        if st == 200 and "#EXT-X-ENDLIST" in text:
            return text, time.time() - t0
        if time.time() - t0 > timeout:
            return text, None
        time.sleep(poll)


# --- MP4 boxes ---------------------------------------------------------------

def boxes(b, start=0, end=None):
    end = len(b) if end is None else end
    i = start
    while i + 8 <= end:
        size, typ = struct.unpack(">I4s", b[i:i + 8])
        hdr = 8
        if size == 1:
            size = struct.unpack(">Q", b[i + 8:i + 16])[0]
            hdr = 16
        elif size == 0:
            size = end - i
        if size < hdr:
            return
        yield typ.decode("latin1"), i + hdr, i + size
        i += size


def find(b, path, start=0, end=None):
    """Payload bounds of the first box along path (list of types)."""
    s, e = start, (len(b) if end is None else end)
    for want in path:
        for typ, ps, pe in boxes(b, s, e):
            if typ == want:
                s, e = ps, pe
                break
        else:
            return None
    return s, e


def sample_entry(init):
    """(fourcc, payload start, payload end) of the first video sample entry."""
    r = find(init, ["moov", "trak", "mdia", "minf", "stbl", "stsd"])
    if not r:
        return None
    s, e = r
    for typ, ps, pe in boxes(init, s + 8, e):
        return typ, ps, pe
    return None


def hvcc_of(init):
    se = sample_entry(init)
    if not se:
        return None, None
    fourcc, ps, pe = se
    # VisualSampleEntry: 6 reserved + 2 dref idx + 70 bytes of visual fields.
    r = find(init, ["hvcC"], ps + 78, pe)
    if not r:
        return fourcc, None
    return fourcc, init[r[0]:r[1]]


def hvcc_fields(h):
    b1 = h[1]
    return {
        "profile_space": b1 >> 6,
        "tier": (b1 >> 5) & 1,
        "profile_idc": b1 & 0x1F,
        "compat": struct.unpack(">I", h[2:6])[0],
        "constraints": h[6:12],
        "level_idc": h[12],
        "length_size": (h[21] & 3) + 1,
    }


def codec_string(fourcc, h):
    """RFC 6381 CODECS of an HEVC track from its hvcC, per ISO/IEC 14496-15
    Annex E -- written from the spec, independently of the Go builder."""
    f = hvcc_fields(h)
    ps = ["", "A", "B", "C"][f["profile_space"]]
    rev = int("{:032b}".format(f["compat"])[::-1], 2)
    cons = list(f["constraints"])
    while cons and cons[-1] == 0:
        cons.pop()
    parts = [fourcc, f"{ps}{f['profile_idc']}", f"{rev:X}", f"{'H' if f['tier'] else 'L'}{f['level_idc']}"]
    parts += [f"{c:X}" for c in cons]
    return ".".join(parts)


def mdhd_timescale(init):
    r = find(init, ["moov", "trak", "mdia", "mdhd"])
    s, _ = r
    ver = init[s]
    return struct.unpack(">I", init[s + 20:s + 24] if ver == 1 else init[s + 12:s + 16])[0]


def first_sample(init, seg):
    """(tfdt seconds, first sample composition offset seconds) of a segment."""
    ts = mdhd_timescale(init)
    r = find(seg, ["moof", "traf"])
    s, e = r
    t = find(seg, ["tfdt"], s, e)
    ver = seg[t[0]]
    base = struct.unpack(">Q", seg[t[0] + 4:t[0] + 12])[0] if ver == 1 else struct.unpack(">I", seg[t[0] + 4:t[0] + 8])[0]
    tr = find(seg, ["trun"], s, e)
    p = tr[0]
    flags = struct.unpack(">I", seg[p:p + 4])[0] & 0xFFFFFF
    q = p + 8
    if flags & 1:
        q += 4
    if flags & 4:
        q += 4
    for fl in (0x100, 0x200, 0x400):
        if flags & fl:
            q += 4
    cto = 0
    if flags & 0x800:
        cto = struct.unpack(">i", seg[q:q + 4])[0]
    return base / ts, cto / ts


def parameter_sets_in_samples(seg, length_size):
    r = find(seg, ["mdat"])
    if not r:
        return -1
    s, e = r
    n = 0
    i = s
    while i + length_size <= e:
        ln = int.from_bytes(seg[i:i + length_size], "big")
        i += length_size
        if ln <= 0 or i + ln > e:
            return -2
        if 32 <= (seg[i] >> 1) & 0x3F <= 34:
            n += 1
        i += ln
    return n


# --- ffprobe (in the tools container) ----------------------------------------

def ffprobe(path_in_w, *args):
    cmd = ["docker", "exec", PREFIX + "-tools", "ffprobe", "-v", "error", *args, "-of", "json", path_in_w]
    out = subprocess.run(cmd, capture_output=True, text=True)
    if out.returncode != 0:
        return {"error": out.stderr.strip()}
    return json.loads(out.stdout or "{}")


def wpath(host_path):
    return "/w/" + os.path.relpath(host_path, W)


def source_keyframes(src, until=None):
    args = ["-select_streams", "v:0", "-skip_frame", "nokey", "-show_entries", "frame=pts_time"]
    if until:
        args += ["-read_intervals", f"%+{until}"]
    j = ffprobe(f"/w/media/{src}", *args)
    return sorted(float(f["pts_time"]) for f in j.get("frames", []))


def source_cues(src):
    j = ffprobe(f"/w/media/{src}", "-select_streams", "s:0", "-show_entries", "packet=pts_time")
    return sorted(float(p["pts_time"]) for p in j.get("packets", []))


def vtt_cues(text):
    out = []
    for m in re.finditer(r"(?m)^(?:(\d+):)?(\d+):(\d+\.\d+) -->", text):
        h = int(m.group(1) or 0)
        out.append(h * 3600 + int(m.group(2)) * 60 + float(m.group(3)))
    return out


def probe_track(path):
    """Streams and packet timestamps of an init+segments file."""
    j = ffprobe(wpath(path), "-show_streams", "-show_entries",
                "packet=pts_time,dts_time,flags:stream=codec_name,codec_tag_string,profile,level,width,height,pix_fmt,color_transfer,color_primaries,channels,sample_rate")
    return j


def monotonic(packets):
    dts = [float(p["dts_time"]) for p in packets if p.get("dts_time") not in (None, "N/A")]
    bad = [(i, dts[i - 1], dts[i]) for i in range(1, len(dts)) if dts[i] <= dts[i - 1]]
    return dts, bad
