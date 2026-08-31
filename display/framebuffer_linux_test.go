//go:build linux

package display

import "testing"

// Tests for packPixel against the Raspberry Pi vc4 fb bitfields
// (rgba 5/11,6/5,5/0, i.e. RGB565 little-endian).
func piBitFields() (shR, shG, shB uint32, mR, mG, mB uint32, oR, oG, oB uint32) {
	red := fbBitfield{Offset: 11, Length: 5}
	green := fbBitfield{Offset: 5, Length: 6}
	blue := fbBitfield{Offset: 0, Length: 5}
	return 8 - red.Length, 8 - green.Length, 8 - blue.Length,
		(1 << red.Length) - 1, (1 << green.Length) - 1, (1 << blue.Length) - 1,
		red.Offset, green.Offset, blue.Offset
}

func TestPackPixelRGB565(t *testing.T) {
	shR, shG, shB, mR, mG, mB, oR, oG, oB := piBitFields()

	cases := []struct {
		r, g, b uint8
		want    uint32
		name    string
	}{
		{0, 0, 0, 0x0000, "black"},
		{255, 255, 255, 0xFFFF, "white"},
		{255, 0, 0, 0xF800, "pure red (5-bit max << 11)"},
		{0, 255, 0, 0x07E0, "pure green (6-bit max << 5)"},
		{0, 0, 255, 0x001F, "pure blue (5-bit max)"},
		{1, 1, 1, 0x0000, "near-black truncates to 0"},
		{0, 64, 0, 0x0200, "mid green: 64>>2 = 16 << 5"},
	}
	for _, c := range cases {
		if got := packPixel(c.r, c.g, c.b, shR, shG, shB, mR, mG, mB, oR, oG, oB); got != c.want {
			t.Errorf("%s: packPixel(%d,%d,%d) = 0x%04X, want 0x%04X", c.name, c.r, c.g, c.b, got, c.want)
		}
	}
}

func TestPackPixelXRGB8888(t *testing.T) {
	// Typical 32-bit XRGB8888 layout (R<<16 | G<<8 | B).
	if got := packPixel(255, 128, 0, 0, 0, 0, 0xFF, 0xFF, 0xFF, 16, 8, 0); got != 0xFF8000 {
		t.Errorf("XRGB8888: got 0x%08X, want 0xFF8000", got)
	}
}
