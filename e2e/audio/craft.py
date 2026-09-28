#!/usr/bin/env python3
"""E-AC-3 copies FFmpeg 8.1.2's mp4 muxer refuses (movenc.c handle_eac3),
made on the host from gen.sh's av_eac3_51.mkv by rewriting its E-AC-3
frames in place (the review's ff/craft.py and ff/patch.py, on the frames
found in the MKV itself):

  av_eac3_multi.mkv    every other frame is independent substream 1: a
                       stream of two independent substreams. movenc
                       refuses its packets from the start.
  av_eac3_corrupt.mkv  frame 400 (12.8 s) has its first 8 bytes zeroed: the
                       header no longer parses, and once the track has
                       samples movenc answers AVERROR_INVALIDDATA.

Both play with the source's frame sizes, so the MKV around them is intact.

  craft.py <media dir>
"""
import os
import sys


def crc16(data):
    crc = 0
    for b in data:
        crc ^= b << 8
        for _ in range(8):
            crc = ((crc << 1) ^ 0x8005) & 0xFFFF if crc & 0x8000 else (crc << 1) & 0xFFFF
    return crc


def frames(buf):
    """Offsets of the E-AC-3 frames in buf: a sync word, an E-AC-3 bsid
    (11..16) and a CRC over the frame that checks out."""
    out = []
    i = 0
    while True:
        i = buf.find(b"\x0b\x77", i)
        if i < 0 or i + 6 > len(buf):
            return out
        bsid = buf[i + 5] >> 3
        n = ((((buf[i + 2] & 7) << 8) | buf[i + 3]) + 1) * 2
        if 11 <= bsid <= 16 and i + n <= len(buf) and crc16(buf[i + 2:i + n]) == 0:
            out.append((i, n))
            i += n
        else:
            i += 1


def set_substream(buf, i, n, sid):
    buf[i + 2] = (buf[i + 2] & 0xC7) | (sid << 3)
    c = crc16(buf[i + 2:i + n - 2])
    buf[i + n - 2] = c >> 8
    buf[i + n - 1] = c & 0xFF
    assert crc16(buf[i + 2:i + n]) == 0


def main(media):
    src = bytearray(open(os.path.join(media, "av_eac3_51.mkv"), "rb").read())
    fr = frames(src)
    # 70 s at 1536 samples per frame, 48 kHz.
    if not 2180 <= len(fr) <= 2195:
        sys.exit(f"found {len(fr)} E-AC-3 frames in av_eac3_51.mkv, want ~2188")
    multi = bytearray(src)
    for k in range(1, len(fr), 2):
        set_substream(multi, *fr[k], 1)
    open(os.path.join(media, "av_eac3_multi.mkv"), "wb").write(multi)
    corrupt = bytearray(src)
    i, _ = fr[400]
    corrupt[i:i + 8] = bytes(8)
    open(os.path.join(media, "av_eac3_corrupt.mkv"), "wb").write(corrupt)
    print(f"craft: {len(fr)} E-AC-3 frames; av_eac3_multi.mkv, av_eac3_corrupt.mkv (frame 400 at {400 * 1536 / 48000:.1f} s)")


if __name__ == "__main__":
    main(sys.argv[1])
