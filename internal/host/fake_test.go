package host

import (
	"bytes"
	"image/jpeg"
	"testing"
)

func TestRenderChart(t *testing.T) {
	b, w, h, err := RenderChart(70, 320, "back", true, 2, "A2B3C4")
	if err != nil {
		t.Fatal(err)
	}
	if w != 320 || h != 180 {
		t.Fatalf("size %dx%d", w, h)
	}
	if b[0] != 0xff || b[1] != 0xd8 {
		t.Fatalf("not jpeg %x", b[:2])
	}
	img, err := jpeg.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != 320 {
		t.Fatalf("decoded width %d", img.Bounds().Dx())
	}
}
