package main

import (
	"os"

	"github.com/wailsapp/wails/v3/pkg/w32"
)

// foreground — чьё окно сейчас активно: заголовок и наше ли оно. По нему видно, забрал ли тост
// фокус у приложения, где человек печатает.
func foreground() (title string, ours bool, ok bool) {
	h := w32.GetForegroundWindow()
	if h == 0 {
		return "", false, true
	}
	_, pid := w32.GetWindowThreadProcessId(h)
	return w32.GetWindowText(h), pid == os.Getpid(), true
}
