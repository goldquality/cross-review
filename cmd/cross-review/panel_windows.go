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
// оно становится активным. Поэтому, когда страница панели загрузилась, панель один раз
// показывается через Show() и сразу прячется (если тостов ещё нет), а фокус возвращается окну, у
// которого был. Дальше панель показывает и прячет panelShow/panelHide, мимо Wails.
func panelWarmUp(w *application.WebviewWindow, visible bool) {
	hwnd := w32.HWND(w.NativeWindow())
	prev := application.InvokeSyncWithResult(w32.GetForegroundWindow)
	w.Show()
	application.InvokeSync(func() {
		if !visible {
			w32.ShowWindow(hwnd, w32.SW_HIDE)
		}
		giveFocusBack(hwnd, prev)
	})
}

func panelShow(w *application.WebviewWindow) {
	hwnd := w32.HWND(w.NativeWindow())
	application.InvokeSync(func() {
		prev := w32.GetForegroundWindow()
		w32.ShowWindow(hwnd, w32.SW_SHOWNOACTIVATE)
		w32.SetWindowPos(hwnd, w32.HWND_TOPMOST, 0, 0, 0, 0, w32.SWP_NOMOVE|w32.SWP_NOSIZE|w32.SWP_NOACTIVATE)
		giveFocusBack(hwnd, prev)
	})
}

func panelHide(w *application.WebviewWindow) {
	hwnd := w32.HWND(w.NativeWindow())
	application.InvokeSync(func() {
		prev := w32.GetForegroundWindow()
		w32.ShowWindow(hwnd, w32.SW_HIDE)
		giveFocusBack(hwnd, prev)
	})
}

// giveFocusBack — если активной стала панель, вернуть фокус окну prev. Панель тогда сама была
// активной, а активному процессу Windows разрешает отдать фокус.
func giveFocusBack(panel, prev w32.HWND) {
	if prev != 0 && prev != panel && w32.GetForegroundWindow() == panel {
		w32.SetForegroundWindow(prev)
	}
}
