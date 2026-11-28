//go:build linux

package main

import "os"

// prepareLinux — на Wayland окно не может само встать в угол экрана и быть поверх остальных: Wails
// делает это через X11 и на Wayland молчит. Поэтому, если есть XWayland, GTK запускается через
// него. PROTO_WAYLAND=1 — остаться на Wayland, чтобы сравнить.
func prepareLinux() {
	if os.Getenv("PROTO_WAYLAND") == "1" || os.Getenv("GDK_BACKEND") != "" {
		return
	}
	if os.Getenv("WAYLAND_DISPLAY") != "" && os.Getenv("DISPLAY") != "" {
		os.Setenv("GDK_BACKEND", "x11")
	}
}
