// These tests are pointed almost entirely at buildICO, because that is the
// hand-rolled binary format. If drawMark regresses you see it the moment you
// open the file; if an offset in the ICONDIR is four bytes out, the icon still
// "exists", still has the right size on disk, and simply stops rendering in one
// browser out of five. So the container is checked against a parser that reads
// the bytes back independently, and the geometry is checked only for the few
// properties a human eye would not reliably catch.
package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/png"
	"testing"
)

func TestBuildICOContainer(t *testing.T) {
	sizes := []int{16, 32, 48}
	frames := make([]*image.NRGBA, 0, len(sizes))
	for _, n := range sizes {
		frames = append(frames, drawMark(n, true))
	}

	raw, err := buildICO(frames)
	if err != nil {
		t.Fatalf("buildICO: %v", err)
	}

	le := binary.LittleEndian
	if got := le.Uint16(raw[0:]); got != 0 {
		t.Errorf("reserved = %d, want 0", got)
	}
	if got := le.Uint16(raw[2:]); got != icoTypeIcon {
		t.Errorf("type = %d, want %d", got, icoTypeIcon)
	}
	if got := int(le.Uint16(raw[4:])); got != len(sizes) {
		t.Fatalf("count = %d, want %d", got, len(sizes))
	}

	// Directory entries: the declared dimensions must match the frames, and the
	// payloads must tile the file after the directory with no gaps and no
	// overlap. A wrong offset is the failure this test exists for.
	wantOffset := icoHeaderSize + icoEntrySize*len(sizes)
	for i, n := range sizes {
		e := raw[icoHeaderSize+i*icoEntrySize:]
		if int(e[0]) != n || int(e[1]) != n {
			t.Errorf("entry %d: declared %dx%d, want %dx%d", i, e[0], e[1], n, n)
		}
		if e[2] != 0 || e[3] != 0 {
			t.Errorf("entry %d: palette/reserved = %d/%d, want 0/0", i, e[2], e[3])
		}
		if got := le.Uint16(e[4:]); got != 1 {
			t.Errorf("entry %d: planes = %d, want 1", i, got)
		}
		if got := le.Uint16(e[6:]); got != 32 {
			t.Errorf("entry %d: bpp = %d, want 32", i, got)
		}
		if got := int(le.Uint32(e[12:])); got != wantOffset {
			t.Errorf("entry %d: offset = %d, want %d", i, got, wantOffset)
		}
		wantOffset += int(le.Uint32(e[8:]))
	}
	if wantOffset != len(raw) {
		t.Errorf("entries account for %d bytes, file is %d", wantOffset, len(raw))
	}

	// And the payloads themselves: real PNGs, at the sizes the directory claims,
	// none of them empty.
	blobs, err := parseICO(raw)
	if err != nil {
		t.Fatalf("parseICO of our own output: %v", err)
	}
	if len(blobs) != len(sizes) {
		t.Fatalf("parseICO returned %d frames, want %d", len(blobs), len(sizes))
	}
	for i, b := range blobs {
		if len(b) == 0 {
			t.Fatalf("frame %d is empty", i)
		}
		img, err := png.Decode(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("frame %d does not decode as PNG: %v", i, err)
		}
		if got := img.Bounds(); got.Dx() != sizes[i] || got.Dy() != sizes[i] {
			t.Errorf("frame %d decodes as %v, want %dx%d", i, got, sizes[i], sizes[i])
		}
	}
}

// A 256px frame is the one dimension the format cannot state literally: the
// byte is 0 and readers are expected to know. Nothing here ships a 256 frame,
// which is exactly why the encoding would rot unnoticed.
func TestBuildICOEncodes256AsZero(t *testing.T) {
	raw, err := buildICO([]*image.NRGBA{image.NewNRGBA(image.Rect(0, 0, 256, 256))})
	if err != nil {
		t.Fatalf("buildICO: %v", err)
	}
	e := raw[icoHeaderSize:]
	if e[0] != 0 || e[1] != 0 {
		t.Errorf("256px frame declared as %dx%d, want 0x0", e[0], e[1])
	}
}

func TestBuildICORejectsBadFrames(t *testing.T) {
	tests := map[string][]*image.NRGBA{
		"no frames":  {},
		"non-square": {image.NewNRGBA(image.Rect(0, 0, 32, 16))},
		"too large":  {image.NewNRGBA(image.Rect(0, 0, 512, 512))},
		"empty":      {image.NewNRGBA(image.Rect(0, 0, 0, 0))},
	}
	for name, frames := range tests {
		if _, err := buildICO(frames); err == nil {
			t.Errorf("%s: buildICO succeeded, want an error", name)
		}
	}
}

func TestParseICORejectsCorruption(t *testing.T) {
	good, err := buildICO([]*image.NRGBA{drawMark(16, true)})
	if err != nil {
		t.Fatalf("buildICO: %v", err)
	}
	le := binary.LittleEndian

	corrupt := map[string]func([]byte){
		"offset past end": func(b []byte) { le.PutUint32(b[icoHeaderSize+12:], uint32(len(b))+1) },
		"size past end":   func(b []byte) { le.PutUint32(b[icoHeaderSize+8:], uint32(len(b))) },
		"offset inside directory": func(b []byte) {
			le.PutUint32(b[icoHeaderSize+12:], 2)
		},
		"wrong type":   func(b []byte) { le.PutUint16(b[2:], 2) },
		"reserved set": func(b []byte) { le.PutUint16(b[0:], 1) },
		"zero count":   func(b []byte) { le.PutUint16(b[4:], 0) },
	}
	for name, mangle := range corrupt {
		b := append([]byte(nil), good...)
		mangle(b)
		if _, err := parseICO(b); err == nil {
			t.Errorf("%s: parseICO accepted it, want an error", name)
		}
	}
	if _, err := parseICO(good[:4]); err == nil {
		t.Error("truncated header: parseICO accepted it, want an error")
	}
}

// The two things about the drawn mark that a glance at the file will not tell
// you: that the rounded plate really is transparent outside its corner radius
// (an opaque corner makes a square blob on rounded browser chrome), and that
// the apple-touch variant really is opaque everywhere (iOS composites
// transparency onto black).
func TestDrawMarkAlpha(t *testing.T) {
	rounded := drawMark(48, true)
	if a := rounded.NRGBAAt(0, 0).A; a != 0 {
		t.Errorf("rounded corner alpha = %d, want 0", a)
	}
	if got := rounded.NRGBAAt(24, 0).A; got != 255 {
		t.Errorf("top-edge midpoint alpha = %d, want 255", got)
	}

	square := drawMark(180, false)
	b := square.Bounds()
	if b.Dx() != 180 || b.Dy() != 180 {
		t.Fatalf("apple-touch icon is %v, want 180x180", b)
	}
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if a := square.NRGBAAt(x, y).A; a != 255 {
				t.Fatalf("apple-touch pixel (%d,%d) alpha = %d, want 255", x, y, a)
			}
		}
	}
	if got := square.NRGBAAt(0, 0); got != plateColor {
		t.Errorf("apple-touch corner = %v, want the plate colour %v", got, plateColor)
	}
}

// Anti-aliasing is the reason for the supersampling pass, and its absence is
// invisible in a passing test suite and glaring in a browser tab. At 16px the
// mark must contain pixels that are neither pure plate nor pure network.
func TestDrawMarkIsAntiAliased(t *testing.T) {
	img := drawMark(16, true)
	var blended int
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := img.NRGBAAt(x, y)
			if c.A == 255 && c != plateColor && c != netColor {
				blended++
			}
		}
	}
	if blended < 32 {
		t.Errorf("only %d blended pixels in a 16x16 frame; the mark is drawing aliased", blended)
	}
}
