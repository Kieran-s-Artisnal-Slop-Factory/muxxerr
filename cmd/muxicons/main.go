// muxicons regenerates the raster icon fallbacks that the gateway's UI needs
// but cannot express as SVG: internal/web/static/favicon.ico (16/32/48 in one
// container) and internal/web/static/icon-180.png (the apple-touch-icon).
//
// Why this exists as a separate command you run by hand
// -----------------------------------------------------
// Everything else in internal/web/static is text you can read in a diff. The
// .ico is not: it is a binary container holding three pre-scaled bitmaps, and
// no amount of staring at it will tell you it has drifted out of sync with
// favicon.svg. So the mark lives in the SVG, and this is the thing you re-run
// whenever the mark changes.
//
// It is deliberately not wired into muxbuild. muxbuild runs on every build and
// exists to compile apps; these two files change roughly never, and a -icons
// flag on a build tool would be a worse API than a command whose name says
// exactly what it does. Keeping it separate also keeps the failure modes apart:
// nothing about regenerating an icon should be able to fail a real build.
//
// Why it draws instead of rasterising
// -----------------------------------
// There is no SVG library here, and the standard library has no vector
// rasteriser. Pulling one in (resvg, a cairo binding, even golang.org/x/image)
// means a dependency — and for one of them a C toolchain — owned forever for
// the sake of two small files that regenerate almost never. So the simplified
// mark is re-drawn directly against the *same coordinate grid* as favicon.svg
// (a 32x32 space). The geometry constants below are transcribed from that file;
// if you move a node there, move it here too.
//
// Anti-aliasing is the whole difficulty at 16px, and a jagged favicon is the
// failure this guards against. Every shape is drawn as a hard-edged coverage
// test at 8x the target size and the result is box-filtered down, which is the
// same approach the Python this replaces took (it drew at 8x and reduced with
// LANCZOS). At an exact integer 1:8 reduction a box filter and a windowed-sinc
// differ only in a hair of edge sharpening, so this is a faithful port rather
// than a visible downgrade — and it costs no dependency. Averaging happens in
// premultiplied alpha, because the rounded plate has genuinely transparent
// corners and averaging straight RGBA there would drag the edge toward black.
//
// Colours are the light-default palette. Rasters cannot answer a media query,
// so they use the variant that survives on both light and dark chrome: a deep
// cyan plate with a near-white network. (The second cyan, #3FD9E4, belongs to
// icon.svg's larger mark only — the simplified favicon mark has two colours,
// because at 16px the second cyan reads as mud.)
//
// Usage:
//
//	go run ./cmd/muxicons
package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"runtime"
)

// --- palette (light default; keep in step with favicon.svg) -----------------
var (
	plateColor = color.NRGBA{R: 0x0E, G: 0x74, B: 0x90, A: 0xFF} // #0E7490 cyan-deep
	netColor   = color.NRGBA{R: 0xEA, G: 0xFB, B: 0xFF, A: 0xFF} // #EAFBFF near-white
	clearColor = color.NRGBA{}
)

// --- geometry, in favicon.svg's 32x32 units ---------------------------------
const (
	grid   = 32.0 // the viewBox every constant below is expressed in
	plateR = 7.0  // rect rx
	stroke = 3.0  // edge stroke-width
	rootX  = 10.0 // the gateway node
	rootY  = 16.0
	rootR  = 4.0
	leafR  = 3.2

	// supersample factor: shapes are rasterised at ss times the output size and
	// averaged back down. 8 gives 64 coverage samples per output pixel, which is
	// more gradation than an 8-bit channel can show.
	ss = 8
)

// leaves are the app instances the gateway fans out to.
var leaves = [3][2]float64{{23.0, 6.5}, {23.0, 16.0}, {23.0, 25.5}}

func main() {
	if err := run(); err != nil {
		// Printed rather than logged: this is a program a human runs at a
		// terminal, and its errors are sentences.
		fmt.Fprintf(os.Stderr, "\nmuxicons: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	dir, err := staticDir()
	if err != nil {
		return err
	}
	outICO := filepath.Join(dir, "favicon.ico")
	outPNG := filepath.Join(dir, "icon-180.png")

	// Each .ico size is drawn natively rather than resized off one master, so
	// the 16px frame gets strokes computed for 16px instead of a squashed 48px
	// one. That is the difference between a legible mark and a smear.
	sizes := []int{16, 32, 48}
	frames := make([]*image.NRGBA, 0, len(sizes))
	for _, n := range sizes {
		frames = append(frames, drawMark(n, true))
	}
	ico, err := buildICO(frames)
	if err != nil {
		return err
	}
	if err := os.WriteFile(outICO, ico, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", outICO, err)
	}

	// rounded=false: iOS applies its own mask to an apple-touch-icon and
	// composites anything transparent onto black, so the plate fills the square
	// and the corner radius is left to the platform.
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, drawMark(180, false)); err != nil {
		return fmt.Errorf("encode %s: %w", outPNG, err)
	}
	if err := os.WriteFile(outPNG, pngBuf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", outPNG, err)
	}

	// Read both back and prove it, for the same reason this file exists: a
	// binary artifact that is subtly wrong looks exactly like one that is right.
	got, err := describeICO(outICO)
	if err != nil {
		return err
	}
	fmt.Printf("%s: %d bytes, frames=%v\n", outICO, len(ico), got)
	cfg, err := decodeConfig(outPNG)
	if err != nil {
		return err
	}
	fmt.Printf("%s: %d bytes, %dx%d\n", outPNG, pngBuf.Len(), cfg.Width, cfg.Height)
	return nil
}

// staticDir resolves internal/web/static from this source file's own location,
// not from the working directory, so `go run ./cmd/muxicons` writes to the same
// place whichever directory you happen to be standing in. The command lives in
// cmd/ rather than beside the SVGs because internal/web/static is embedded into
// the gateway binary and served on the web: a generator has no business being
// either.
func staticDir() (string, error) {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("cannot determine this file's location; build without -trimpath")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(self))) // cmd/muxicons/main.go -> repo root
	dir := filepath.Join(root, "internal", "web", "static")
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return "", fmt.Errorf("static directory %s not found (resolved from %s); "+
			"this command writes into the source tree and must be run from a checkout", dir, self)
	}
	return dir, nil
}

// drawMark renders the simplified mark at px by px.
//
// rounded=false fills the whole square with the plate colour and skips the
// corner radius; see the apple-touch note in run.
func drawMark(px int, rounded bool) *image.NRGBA {
	s := px * ss
	k := float64(s) / grid // svg units -> supersampled pixels

	big := image.NewNRGBA(image.Rect(0, 0, s, s))
	if rounded {
		fill(big, clearColor)
		fillRoundedRect(big, float64(s), plateR*k, plateColor)
	} else {
		fill(big, plateColor)
	}

	// Edges: gateway -> each instance. The ends are buried under the node
	// discs, so butt caps never show and the SVG's stroke-linecap="round" has
	// nothing to do here.
	width := math.Max(1, math.Round(stroke*k))
	for _, l := range leaves {
		fillSegment(big, rootX*k, rootY*k, l[0]*k, l[1]*k, width, netColor)
	}

	// Nodes.
	fillDisc(big, rootX*k, rootY*k, rootR*k, netColor)
	for _, l := range leaves {
		fillDisc(big, l[0]*k, l[1]*k, leafR*k, netColor)
	}

	return downsample(big, ss)
}

func fill(img *image.NRGBA, c color.NRGBA) {
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		row := img.Pix[img.PixOffset(b.Min.X, y) : img.PixOffset(b.Max.X-1, y)+4]
		for i := 0; i < len(row); i += 4 {
			row[i], row[i+1], row[i+2], row[i+3] = c.R, c.G, c.B, c.A
		}
	}
}

// fillRoundedRect fills [0,size]^2 with corner radius r. Coverage is a hard
// in/out test at each sample centre — the smoothing comes from downsample, not
// from here, which is what keeps every shape's edge quality identical.
func fillRoundedRect(img *image.NRGBA, size, r float64, c color.NRGBA) {
	forEachSample(img, 0, 0, size, size, func(x, y float64) bool {
		// Clamp the sample into the inner rectangle whose corners are the
		// circle centres; the distance to that point is the rounded-rect
		// distance.
		cx := math.Min(math.Max(x, r), size-r)
		cy := math.Min(math.Max(y, r), size-r)
		dx, dy := x-cx, y-cy
		return dx*dx+dy*dy <= r*r
	}, c)
}

func fillDisc(img *image.NRGBA, cx, cy, r float64, c color.NRGBA) {
	forEachSample(img, cx-r, cy-r, cx+r, cy+r, func(x, y float64) bool {
		dx, dy := x-cx, y-cy
		return dx*dx+dy*dy <= r*r
	}, c)
}

// fillSegment fills the w-wide rectangle swept along p0->p1, with butt caps.
func fillSegment(img *image.NRGBA, x0, y0, x1, y1, w float64, c color.NRGBA) {
	dx, dy := x1-x0, y1-y0
	length := math.Hypot(dx, dy)
	if length == 0 {
		return
	}
	ux, uy := dx/length, dy/length // along the segment
	half := w / 2
	forEachSample(img,
		math.Min(x0, x1)-half, math.Min(y0, y1)-half,
		math.Max(x0, x1)+half, math.Max(y0, y1)+half,
		func(x, y float64) bool {
			px, py := x-x0, y-y0
			along := px*ux + py*uy
			if along < 0 || along > length {
				return false
			}
			return math.Abs(px*uy-py*ux) <= half
		}, c)
}

// forEachSample tests the centre of every pixel in the given bounding box and
// writes c where inside reports true. Every shape goes through here so that
// they all share one sampling convention: sample at (x+0.5, y+0.5).
func forEachSample(img *image.NRGBA, minX, minY, maxX, maxY float64, inside func(x, y float64) bool, c color.NRGBA) {
	b := img.Bounds()
	x0 := clampInt(int(math.Floor(minX)), b.Min.X, b.Max.X)
	y0 := clampInt(int(math.Floor(minY)), b.Min.Y, b.Max.Y)
	x1 := clampInt(int(math.Ceil(maxX))+1, b.Min.X, b.Max.X)
	y1 := clampInt(int(math.Ceil(maxY))+1, b.Min.Y, b.Max.Y)
	for y := y0; y < y1; y++ {
		fy := float64(y) + 0.5
		for x := x0; x < x1; x++ {
			if !inside(float64(x)+0.5, fy) {
				continue
			}
			i := img.PixOffset(x, y)
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, c.A
		}
	}
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// downsample box-filters an n-times-oversampled image back to its nominal size.
//
// The averaging is done on premultiplied values and undone afterwards. The
// straight-alpha alternative would average the RGB of transparent pixels — which
// are (0,0,0) — into the edge of the plate and ring the rounded corners with
// grey, which is exactly the artefact this whole supersampling dance is meant
// to avoid.
func downsample(src *image.NRGBA, n int) *image.NRGBA {
	b := src.Bounds()
	w, h := b.Dx()/n, b.Dy()/n
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	area := uint32(n * n)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var sr, sg, sb, sa uint32
			for dy := 0; dy < n; dy++ {
				for dx := 0; dx < n; dx++ {
					i := src.PixOffset(x*n+dx, y*n+dy)
					a := uint32(src.Pix[i+3])
					sr += uint32(src.Pix[i+0]) * a
					sg += uint32(src.Pix[i+1]) * a
					sb += uint32(src.Pix[i+2]) * a
					sa += a
				}
			}
			o := dst.PixOffset(x, y)
			if sa == 0 {
				dst.Pix[o], dst.Pix[o+1], dst.Pix[o+2], dst.Pix[o+3] = 0, 0, 0, 0
				continue
			}
			// +sa/2 rounds to nearest rather than truncating; over a 16px icon
			// the truncation is a visible half-shade darker.
			dst.Pix[o+0] = uint8((sr + sa/2) / sa)
			dst.Pix[o+1] = uint8((sg + sa/2) / sa)
			dst.Pix[o+2] = uint8((sb + sa/2) / sa)
			dst.Pix[o+3] = uint8((sa + area/2) / area)
		}
	}
	return dst
}

// --- the .ico container -----------------------------------------------------

// An .ico is an ICONDIR (6 bytes: reserved, type, count) followed by one
// 16-byte ICONDIRENTRY per frame, followed by the frames' payloads. The entry
// says how big the payload is and where it starts, and the payload is either a
// headerless BMP or — since Windows Vista, and in every browser that matters —
// a whole PNG file, verbatim.
//
// PNG is what this writes. The BMP form is a bottom-up DIB with a doubled
// height and a vestigial 1-bit AND mask that nothing has consulted in twenty
// years, and hand-rolling it would be more code and more ways to be silently
// wrong. The cost is that Windows XP would not render these; that is not a
// platform this gateway's UI targets.
const (
	icoHeaderSize = 6
	icoEntrySize  = 16
	icoTypeIcon   = 1
)

func buildICO(frames []*image.NRGBA) ([]byte, error) {
	if len(frames) == 0 {
		return nil, errors.New("ico: no frames")
	}
	if len(frames) > 0xFFFF {
		return nil, fmt.Errorf("ico: %d frames exceeds the uint16 count field", len(frames))
	}

	blobs := make([][]byte, len(frames))
	dims := make([]int, len(frames))
	for i, f := range frames {
		b := f.Bounds()
		if b.Dx() != b.Dy() {
			return nil, fmt.Errorf("ico: frame %d is %dx%d; icon frames must be square", i, b.Dx(), b.Dy())
		}
		// The width and height fields are single bytes, with 0 meaning 256.
		// Anything outside 1..256 simply cannot be described by this format.
		if b.Dx() < 1 || b.Dx() > 256 {
			return nil, fmt.Errorf("ico: frame %d is %dpx; must be 1..256", i, b.Dx())
		}
		var enc bytes.Buffer
		if err := png.Encode(&enc, f); err != nil {
			return nil, fmt.Errorf("ico: encode frame %d: %w", i, err)
		}
		blobs[i], dims[i] = enc.Bytes(), b.Dx()
	}

	le := binary.LittleEndian
	out := make([]byte, icoHeaderSize+icoEntrySize*len(frames))
	le.PutUint16(out[0:], 0) // reserved, always zero
	le.PutUint16(out[2:], icoTypeIcon)
	le.PutUint16(out[4:], uint16(len(frames)))

	offset := len(out)
	for i := range frames {
		e := out[icoHeaderSize+i*icoEntrySize:]
		e[0] = byte(dims[i] % 256) // width;  256 wraps to 0, which is the encoding
		e[1] = byte(dims[i] % 256) // height
		e[2] = 0                   // palette entries; 0 for a non-paletted image
		e[3] = 0                   // reserved
		le.PutUint16(e[4:], 1)     // colour planes
		le.PutUint16(e[6:], 32)    // bits per pixel
		le.PutUint32(e[8:], uint32(len(blobs[i])))
		le.PutUint32(e[12:], uint32(offset))
		offset += len(blobs[i])
	}
	for _, b := range blobs {
		out = append(out, b...)
	}
	return out, nil
}

// describeICO re-reads a written .ico and returns the frame sizes it actually
// contains, by decoding each payload rather than trusting the directory — the
// directory is exactly the part that can lie.
func describeICO(path string) ([]int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	entries, err := parseICO(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	sizes := make([]int, 0, len(entries))
	for i, e := range entries {
		cfg, err := png.DecodeConfig(bytes.NewReader(e))
		if err != nil {
			return nil, fmt.Errorf("%s: frame %d does not decode as PNG: %w", path, i, err)
		}
		if cfg.Width != cfg.Height {
			return nil, fmt.Errorf("%s: frame %d is %dx%d", path, i, cfg.Width, cfg.Height)
		}
		sizes = append(sizes, cfg.Width)
	}
	return sizes, nil
}

// parseICO returns each frame's payload bytes. It is the reader half of
// buildICO and exists so the writer can be checked against something other than
// itself.
func parseICO(raw []byte) ([][]byte, error) {
	if len(raw) < icoHeaderSize {
		return nil, fmt.Errorf("truncated: %d bytes", len(raw))
	}
	le := binary.LittleEndian
	if got := le.Uint16(raw[0:]); got != 0 {
		return nil, fmt.Errorf("reserved field is %d, want 0", got)
	}
	if got := le.Uint16(raw[2:]); got != icoTypeIcon {
		return nil, fmt.Errorf("type is %d, want %d (icon)", got, icoTypeIcon)
	}
	count := int(le.Uint16(raw[4:]))
	if count == 0 {
		return nil, errors.New("no frames")
	}
	if len(raw) < icoHeaderSize+icoEntrySize*count {
		return nil, fmt.Errorf("truncated: %d bytes for %d entries", len(raw), count)
	}
	out := make([][]byte, 0, count)
	for i := 0; i < count; i++ {
		e := raw[icoHeaderSize+i*icoEntrySize:]
		size := int(le.Uint32(e[8:]))
		off := int(le.Uint32(e[12:]))
		if off < icoHeaderSize+icoEntrySize*count || size <= 0 || off+size > len(raw) {
			return nil, fmt.Errorf("entry %d points at [%d,%d) which is outside the %d-byte file", i, off, off+size, len(raw))
		}
		out = append(out, raw[off:off+size])
	}
	return out, nil
}

func decodeConfig(path string) (image.Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return image.Config{}, err
	}
	defer f.Close()
	cfg, err := png.DecodeConfig(f)
	if err != nil {
		return image.Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}
