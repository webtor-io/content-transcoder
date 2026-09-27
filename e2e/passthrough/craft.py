"""Adds a Dolby Vision configuration record (dvcC) to the first sample entry
of an MP4 whose hvc1/hev1 track comes first and whose moov is at the end
(so no chunk offset moves): a stand-in for a DV source, which FFmpeg cannot
make. FFmpeg's mov demuxer reads it as the DOVI side data ffprobe reports.

  python3 craft.py <in.mp4> <out.mp4> <profile> <level> <bl_compatibility_id>
"""
import struct
import sys


def boxes(buf, start, end):
    i = start
    while i < end:
        size, typ = struct.unpack('>I4s', buf[i:i + 8])
        hdr = 8
        if size == 1:
            size = struct.unpack('>Q', buf[i + 8:i + 16])[0]
            hdr = 16
        yield i, size, typ.decode('latin1'), hdr
        i += size


def add_dvcc(src, dst, profile, level, compat, rpu=1, el=0, bl=1):
    buf = bytearray(open(src, 'rb').read())

    def find(start, end, want):
        for off, size, typ, hdr in boxes(buf, start, end):
            if typ == want:
                return off, size, hdr
        raise SystemExit('no ' + want)

    cur = find(0, len(buf), 'moov')
    chain = [cur]
    for name in ['trak', 'mdia', 'minf', 'stbl', 'stsd']:
        off, size, hdr = cur
        cur = find(off + hdr, off + size, name)
        chain.append(cur)
    # stsd: full box header (4) + entry count (4), then the sample entries.
    off, size, hdr = cur
    eoff, esize, etyp, ehdr = next(boxes(buf, off + hdr + 8, off + size))
    assert etyp in ('hvc1', 'hev1'), etyp
    chain.append((eoff, esize, ehdr))
    rec = bytes([1, 0]) + struct.pack('>H', (profile << 9) | (level << 3) | (rpu << 2) | (el << 1) | bl) + bytes([compat << 4]) + bytes(19)
    box = struct.pack('>I4s', 8 + len(rec), b'dvcC') + rec
    ins = eoff + esize
    buf[ins:ins] = box
    for o, s, h in chain:
        assert h == 8
        struct.pack_into('>I', buf, o, s + len(box))
    open(dst, 'wb').write(buf)


if __name__ == '__main__':
    add_dvcc(sys.argv[1], sys.argv[2], int(sys.argv[3]), int(sys.argv[4]), int(sys.argv[5]))
