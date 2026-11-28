//go:build !windows

package main

import "github.com/wailsapp/wails/v3/pkg/application"

func toastExStyle() int { return 0 }

// showWithoutFocus показывает тост. На macOS окно тоста — панель NonActivating, и Show() Wails
// выводит её через orderFrontRegardless, не делая ключевым окном.
func showWithoutFocus(w *application.WebviewWindow) { w.Show() }
