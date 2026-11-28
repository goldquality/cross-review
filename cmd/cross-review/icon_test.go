package main

import (
	"bytes"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/goldquality/cross-review/internal/client"
)

var stateNames = map[trayState]string{stateOnline: "online", stateOffline: "offline", statePaused: "paused", stateReviewing: "reviewing"}

// decodeIcon разбирает иконку трея: на Windows это PNG внутри ICO — заголовок 22 байта, потом PNG.
func decodeIcon(t *testing.T, b []byte) image.Image {
	t.Helper()
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
	return img
}

func TestTrayIcon(t *testing.T) {
	for s, name := range stateNames {
		b := trayIcon(s)
		img := decodeIcon(t, b)
		if img.Bounds().Dx() != iconSize {
			t.Fatal(name, img.Bounds())
		}
		// Кружок со знаком в правом нижнем углу — только на паузе и во время ревью.
		_, _, _, a := img.At(iconSize-4, iconSize-16).RGBA()
		if badge := s == statePaused || s == stateReviewing; badge != (a > 0) {
			t.Errorf("%s: кружок в углу — %v", name, a > 0)
		}
		// Пауза и ревью цветные везде, на macOS остальные — шаблоны.
		if colored := s == statePaused || s == stateReviewing; colored == iconTemplate(s) && runtime.GOOS == "darwin" {
			t.Errorf("%s: шаблон %v", name, iconTemplate(s))
		}
		if d := os.Getenv("ICON_DUMP"); d != "" {
			os.WriteFile(filepath.Join(d, name+".png"), encodePNG(img), 0o644)
		}
	}
	if bytes.Equal(trayIcon(statePaused), trayIcon(stateReviewing)) {
		t.Fatal("пауза и ревью неотличимы")
	}
	ico := pngToICO([]byte{1, 2, 3}, 64)
	if len(ico) != 22+3 || ico[2] != 1 || ico[4] != 1 {
		t.Fatal(ico)
	}
}

func TestIconState(t *testing.T) {
	paused := time.Now().Add(time.Hour)
	cur := &client.Running{Short: "shop!9"}
	for _, c := range []struct {
		st   client.State
		want trayState
	}{
		{client.State{Configured: true, Online: true}, stateOnline},
		{client.State{Configured: true}, stateOffline},
		{client.State{}, stateOffline},
		{client.State{Configured: true, Online: true, PausedUntil: paused}, statePaused},
		{client.State{Configured: true, PausedUntil: paused}, statePaused}, // пауза видна и без связи
		{client.State{Configured: true, Online: true, Current: cur}, stateReviewing},
		{client.State{Configured: true, Online: true, Current: cur, PausedUntil: paused}, stateReviewing}, // пауза не останавливает начатое ревью
		{client.State{Configured: true, Current: cur}, stateReviewing},                                    // ревью идёт и без связи
	} {
		if got := iconState(c.st); got != c.want {
			t.Errorf("%s: %+v → %s", stateNames[c.want], c.st, stateNames[got])
		}
	}
}
