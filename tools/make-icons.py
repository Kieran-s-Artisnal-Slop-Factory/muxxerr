#!/usr/bin/env python3
"""
Regenerate the raster icon fallbacks: favicon.ico and icon-180.png.

Why this file exists at all
---------------------------
Everything else in this directory is text you can read in a diff. The .ico is
not: it is a binary container holding three pre-scaled bitmaps, and no amount
of staring at it will tell you it has drifted out of sync with favicon.svg.
So the mark lives in the SVG, and this script is the thing you re-run by hand
whenever the mark changes. It is not wired into a build — the gateway compiles
with the Go toolchain alone and that stays true.

Why it draws instead of rasterising
-----------------------------------
There is no SVG library here. cairosvg is not installed and pulling it in
means a C toolchain and a libcairo, which is a lot of machinery to own for two
small files. Pillow is available, so the simplified mark is re-drawn directly
with ImageDraw against the *same coordinate grid* as favicon.svg (a 32x32
space). The geometry constants below are transcribed from that file; if you
move a node there, move it here too. Anti-aliasing comes from drawing at 8x
and downsampling with LANCZOS, since ImageDraw itself has none.

Colours are the light-default palette. Rasters cannot answer a media query, so
they use the variant that survives on both light and dark chrome: a deep cyan
plate with a near-white network.
"""

from pathlib import Path

from PIL import Image, ImageDraw

# Written next to the SVGs they accompany, resolved from this file's location
# so the script works from any working directory. It lives in tools/ rather
# than beside them because internal/web/static is embedded into the binary and
# served on the web: a build script has no business being either.
_STATIC = Path(__file__).resolve().parent.parent / "internal" / "web" / "static"
OUT_ICO = str(_STATIC / "favicon.ico")
OUT_PNG = str(_STATIC / "icon-180.png")

# --- palette (light default; keep in step with favicon.svg) ----------------
PLATE = (0x0E, 0x74, 0x90, 0xFF)   # #0E7490 cyan-deep
NET = (0xEA, 0xFB, 0xFF, 0xFF)     # #EAFBFF near-white
CLEAR = (0, 0, 0, 0)

# --- geometry, in favicon.svg's 32x32 units --------------------------------
GRID = 32.0
PLATE_R = 7.0          # rect rx
STROKE = 3.0           # edge stroke-width
ROOT = (10.0, 16.0)    # the gateway node
ROOT_R = 4.0
LEAVES = [(23.0, 6.5), (23.0, 16.0), (23.0, 25.5)]  # the app instances
LEAF_R = 3.2

SS = 8  # supersample factor


def draw_mark(px, rounded=True):
    """Draw the simplified mark at px by px. Supersampled, then reduced.

    rounded=False fills the whole square with the plate colour and skips the
    corner radius: that is what an apple-touch-icon wants, because iOS applies
    its own mask and composites anything transparent onto black.
    """
    s = px * SS
    k = s / GRID  # svg units -> supersampled pixels

    img = Image.new("RGBA", (s, s), PLATE if not rounded else CLEAR)
    d = ImageDraw.Draw(img)

    if rounded:
        d.rounded_rectangle([0, 0, s - 1, s - 1], radius=PLATE_R * k, fill=PLATE)

    # edges: gateway -> each instance. Ends are buried under the node discs,
    # so butt caps (all ImageDraw offers) never show.
    for lx, ly in LEAVES:
        d.line(
            [(ROOT[0] * k, ROOT[1] * k), (lx * k, ly * k)],
            fill=NET,
            width=max(1, round(STROKE * k)),
        )

    # nodes
    def disc(cx, cy, r):
        d.ellipse([(cx - r) * k, (cy - r) * k, (cx + r) * k, (cy + r) * k], fill=NET)

    disc(*ROOT, ROOT_R)
    for lx, ly in LEAVES:
        disc(lx, ly, LEAF_R)

    return img.resize((px, px), Image.Resampling.LANCZOS)


def main():
    # .ico: each size drawn natively rather than resized off one master, so the
    # 16px frame gets strokes computed for 16px instead of a squashed 48px one.
    # Pillow silently drops any requested size larger than the *base* image, so
    # the base has to be the biggest frame and the smaller ones get appended.
    frames = {n: draw_mark(n) for n in (16, 32, 48)}
    frames[48].save(
        OUT_ICO,
        format="ICO",
        sizes=[(16, 16), (32, 32), (48, 48)],
        append_images=[frames[16], frames[32]],
    )

    draw_mark(180, rounded=False).convert("RGB").save(OUT_PNG, format="PNG")

    # read back and prove it
    with Image.open(OUT_ICO) as im:
        got = sorted(getattr(im, "ico", None).sizes()) if hasattr(im, "ico") else None
        print(f"{OUT_ICO}: {im.format} {im.mode} frames={got}")
    with Image.open(OUT_PNG) as im:
        print(f"{OUT_PNG}: {im.format} {im.mode} {im.size}")


if __name__ == "__main__":
    main()
