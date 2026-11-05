"""Dump Atrium's notification sprites (mascot_icons.py) as JSON, for the
icons.js port's node test (web/test/mascots-themes-b.test.mjs):

    python3 -I -B web/test/fixtures/mascots/icons/gen-icons.py > web/test/fixtures/mascots/icons/sprites.json

Atrium is only read (-B: no bytecode written). ATRIUM_SCRIPTS overrides the
scripts folder (default ~/Github/Atrium/home/.config/hypr/scripts). PIL is
not needed: only the sprite tables are read."""
import json
import os
import sys

scripts = os.environ.get("ATRIUM_SCRIPTS") or os.path.expanduser("~/Github/Atrium/home/.config/hypr/scripts")
sys.path.insert(0, scripts)
import mascot_icons as mi  # noqa: E402

out = {
    "grid": mi.GRID,
    "scale": mi.SCALE,
    "moods": list(mi.MOODS),
    "sprites": {t: {"palette": f()[0], "rows": f()[1]} for t, f in mi.SPRITES.items()},
    "signs": {m: {"palette": pal, "rows": list(rows)} for m, (pal, rows) in mi.SIGNS.items()},
}
json.dump(out, sys.stdout, indent=1, sort_keys=True)
sys.stdout.write("\n")
