package main

import (
	"os"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/w32"
)

// toastExStyle — тост поверх всех, без кнопки на панели задач и не становится активным окном.
func toastExStyle() int { return w32.WS_EX_NOACTIVATE | w32.WS_EX_TOOLWINDOW | w32.WS_EX_TOPMOST }

// showWithoutFocus показывает тост. Wails показывает окно через SW_SHOW, а оно делает окно
// активным даже с WS_EX_NOACTIVATE. Поэтому сначала окно становится видимым через
// SW_SHOWNOACTIVATE, а Show() Wails лишь включает WebView. PROTO_WIN_SHOW=wails — только Show(),
// чтобы сравнить.
func showWithoutFocus(w *application.WebviewWindow) {
	if os.Getenv("PROTO_WIN_SHOW") != "wails" {
		hwnd := w32.HWND(w.NativeWindow())
		application.InvokeSync(func() { w32.ShowWindow(hwnd, w32.SW_SHOWNOACTIVATE) })
	}
	w.Show()
}
