#!/usr/bin/env python3
"""Regenerate web/dist/assets/js/mascots/eaw.js: the code point ranges the
mascot grid treats as two columns (east_asian_width W, F or A) or zero
(a non-zero canonical combining class), from this Python's unicodedata, so
the JS Canvas refuses exactly the characters Atrium's mascot_common.dw does.

    python3 -I -B web/test/fixtures/mascots/gen-eaw.py
"""

import os
import unicodedata

OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                   "..", "..", "..", "dist", "assets", "js", "mascots", "eaw.js")


def ranges(pred):
    out, start = [], None
    for cp in range(0x110000):
        ok = not (0xD800 <= cp < 0xE000) and pred(chr(cp))
        if ok and start is None:
            start = cp
        if not ok and start is not None:
            out.append((start, cp - 1))
            start = None
    if start is not None:
        out.append((start, 0x10FFFF))
    return out


def fmt(name, rs):
    flat = [v for pair in rs for v in pair]
    lines, cur = [], ""
    for v in flat:
        s = "0x%x," % v
        if len(cur) + len(s) > 110:
            lines.append(cur)
            cur = ""
        cur += s
    lines.append(cur)
    return "export const %s = [\n%s\n];\n" % (name, "\n".join("  " + l for l in lines))


HEADER = """// Unicode %s character tables for the mascot grid (mascot_common.dw): the
// code point ranges, as flat [first, last, first, last, ...] pairs, whose
// east_asian_width is W, F or A (two columns in a CJK monospace font) and those
// with a non-zero canonical combining class (zero columns). Generated from
// Python's unicodedata so the JS grid refuses exactly the characters Atrium's
// does; regenerate with web/test/fixtures/mascots/gen-eaw.py.

"""


def main():
    wide = ranges(lambda c: unicodedata.east_asian_width(c) in ("W", "F", "A"))
    comb = ranges(lambda c: unicodedata.combining(c) != 0)
    with open(OUT, "w") as fh:
        fh.write(HEADER % unicodedata.unidata_version + fmt("WIDE", wide) + "\n" + fmt("COMBINING", comb))


if __name__ == "__main__":
    main()
