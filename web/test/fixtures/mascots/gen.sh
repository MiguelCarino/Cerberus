#!/usr/bin/env bash
# Generate golden mascot frames from Atrium's Python mascots, for the JS ports'
# node tests (web/test/mascots-*.test.mjs).
#
#   web/test/fixtures/mascots/gen.sh THEME [THEME...]
#
# For each frame line in frames.txt (plus frames-THEME.txt when present) it runs
#
#   python3 -I -B mascots.py --theme THEME --frame STATE TOOL|- TICK COLS ROWS \
#           [--mood M] [--scene S]
#
# and writes, next to this script:
#
#   THEME.frames.txt   "=== STATE TOOL TICK COLS ROWS MOOD SCENE" then the
#                      --frame output verbatim: ROWS lines "|row|", then the
#                      bubble line
#   THEME.markup.txt   the same headers, then "@head X Y" (or "@head -": the
#                      emote anchor the render leaves) and the frame's Pango
#                      markup, one line per row: the colours
#
# Atrium is only read: -I keeps the environment and user site out, -B keeps
# Python from writing bytecode into its __pycache__. ATRIUM_SCRIPTS overrides
# the scripts folder (default ~/Github/Atrium/home/.config/hypr/scripts).
# OUT=DIR writes the fixtures (and reads frames-THEME.txt) there instead of
# next to this script, for per-theme folders; FRAMES=FILE replaces the shared
# frames.txt. Pass HYPR_SEASON=<season> for one seasons palette.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPTS="${ATRIUM_SCRIPTS:-$HOME/Github/Atrium/home/.config/hypr/scripts}"
MASCOTS="$SCRIPTS/mascots.py"
OUTDIR="${OUT:-$HERE}"
FRAMESFILE="${FRAMES:-$HERE/frames.txt}"

if [ $# -lt 1 ]; then
  echo "usage: $0 THEME [THEME...]" >&2
  exit 2
fi
if [ ! -f "$MASCOTS" ]; then
  echo "gen.sh: no $MASCOTS (set ATRIUM_SCRIPTS)" >&2
  exit 1
fi

# frame_specs THEME: the frame lines, comments and blanks dropped.
frame_specs() {
  local files=("$FRAMESFILE")
  [ -f "$OUTDIR/frames-$1.txt" ] && files+=("$OUTDIR/frames-$1.txt")
  grep -hv '^[[:space:]]*\(#\|$\)' "${files[@]}"
}

for theme in "$@"; do
  frames="$OUTDIR/$theme.frames.txt"
  markup="$OUTDIR/$theme.markup.txt"
  tmp_frames="$(mktemp "$frames.XXXXXX")"
  tmp_markup="$(mktemp "$markup.XXXXXX")"
  trap 'rm -f "$tmp_frames" "$tmp_markup"' EXIT
  n=0
  while read -r state tool tick cols rows mood scene _; do
    mood="${mood:--}"
    scene="${scene:--}"
    args=(--theme "$theme" --frame "$state" "$tool" "$tick" "$cols" "$rows")
    [ "$mood" != "-" ] && args+=(--mood "$mood")
    [ "$scene" != "-" ] && args+=(--scene "$scene")
    printf '=== %s %s %s %s %s %s %s\n' "$state" "$tool" "$tick" "$cols" "$rows" "$mood" "$scene" >>"$tmp_frames"
    python3 -I -B "$MASCOTS" "${args[@]}" >>"$tmp_frames"
    n=$((n + 1))
  done < <(frame_specs "$theme")

  # The colours: mascots.py has no markup flag, so one read-only Python reads
  # the same frame lines and prints each frame's markup through its API.
  frame_specs "$theme" | python3 -I -B -c '
import sys
sys.path.insert(0, sys.argv[1])
import mascots
theme = sys.argv[2]
for line in sys.stdin:
    f = line.split()
    state, tool, tick, cols, rows = f[0], f[1], int(f[2]), int(f[3]), int(f[4])
    mood = f[5] if len(f) > 5 else "-"
    scene = f[6] if len(f) > 6 else "-"
    m = mascots.Mascot(cols, rows, theme=theme)
    mk = m.markup(state, tick, None if tool == "-" else tool,
                  None if mood == "-" else mood,
                  frozenset() if scene == "-" else frozenset(scene.split(",")))
    head = getattr(m, "head", None)
    print("=== %s %s %d %d %d %s %s" % (state, tool, tick, cols, rows, mood, scene))
    print("@head %d %d" % tuple(head) if head else "@head -")
    print(mk)
' "$SCRIPTS" "$theme" >"$tmp_markup"

  chmod 644 "$tmp_frames" "$tmp_markup"
  mv "$tmp_frames" "$frames"
  mv "$tmp_markup" "$markup"
  trap - EXIT
  echo "gen.sh: $theme: $n frames -> $(basename "$frames"), $(basename "$markup")" \
    "($(($(stat -c %s "$frames") + $(stat -c %s "$markup"))) bytes)"
done
