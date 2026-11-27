package main

import (
	"bytes"
	"image/png"
	"os"
	"testing"
)

func TestTrayIconIsPNG(t *testing.T) {
	for _, idle := range []bool{false, true} {
		b := trayIcon(idle)
		img, err := png.Decode(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		if img.Bounds().Dx() != 64 {
			t.Fatal(img.Bounds())
		}
		if d := os.Getenv("ICON_DUMP"); d != "" {
			name := "/active.png"
			if idle {
				name = "/idle.png"
			}
			os.WriteFile(d+name, b, 0o644)
		}
	}
	ico := pngToICO([]byte{1, 2, 3}, 64)
	if len(ico) != 22+3 || ico[2] != 1 || ico[4] != 1 {
		t.Fatal(ico)
	}
}
