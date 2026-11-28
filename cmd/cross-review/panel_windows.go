package main

import (
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/w32"
)

// panelOptions — панель тостов на Windows: поверх всех, без кнопки на панели задач, клик по ней не
// делает её активной (WS_EX_NOACTIVATE), так что фокус остаётся в окне, где человек печатает.
func panelOptions(o *application.WebviewWindowOptions) {
	o.Windows = application.WindowsWindow{
		HiddenOnTaskbar: true,
		ExStyle:         w32.WS_EX_NOACTIVATE | w32.WS_EX_TOOLWINDOW | w32.WS_EX_TOPMOST,
	}
}

// panelWarmUp: Wails включает WebView только в Show(), а Show() показывает окно через SW_SHOW, и
// оно становится активным. Поэтому при запуске клиента панель один раз показывается через Show()
// и сразу прячется, а дальше её показывает и прячет panelShow/panelHide, мимо Wails.
func panelWarmUp(w *application.WebviewWindow) {
	w.Show()
	hwnd := w32.HWND(w.NativeWindow())
	application.InvokeSync(func() { w32.ShowWindow(hwnd, w32.SW_HIDE) })
}

func panelShow(w *application.WebviewWindow) {
	hwnd := w32.HWND(w.NativeWindow())
	application.InvokeSync(func() {
		w32.ShowWindow(hwnd, w32.SW_SHOWNOACTIVATE)
		w32.SetWindowPos(hwnd, w32.HWND_TOPMOST, 0, 0, 0, 0, w32.SWP_NOMOVE|w32.SWP_NOSIZE|w32.SWP_NOACTIVATE)
	})
}

func panelHide(w *application.WebviewWindow) {
	hwnd := w32.HWND(w.NativeWindow())
	application.InvokeSync(func() { w32.ShowWindow(hwnd, w32.SW_HIDE) })
}
