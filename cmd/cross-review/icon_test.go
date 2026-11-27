package main

import (
	"bytes"
	"image/png"
	"os"
	"runtime"
	"testing"
)

// На Windows иконка — PNG внутри ICO: заголовок 22 байта, потом сам PNG.
func TestTrayIconIsPNG(t *testing.T) {
	for _, idle := range []bool{false, true} {
		b := trayIcon(idle)
		if runtime.GOOS == "windows" {
			if len(b) < 22 || b[2] != 1 || b[4] != 1 {
				t.Fatal("не ICO")
			}
			b = b[22:]
		}
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
