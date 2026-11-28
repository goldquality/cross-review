package main

import (
	"sync"

	"github.com/ebitengine/purego"
	"github.com/wailsapp/wails/v3/pkg/application"
)

func panelOptions(*application.WebviewWindowOptions) {}

func panelWarmUp(*application.WebviewWindow, bool) {}

// gtk — несколько функций GTK 4, которых нет в Wails. Show() в Wails зовёт gtk_window_present, а
// тот просит оконный менеджер сделать окно активным: тост забирал бы фокус. Здесь окно
// показывается через gtk_widget_set_visible, а перед первым показом получает _NET_WM_USER_TIME = 0 —
// по EWMH это «не давать фокус при появлении». Библиотеки уже загружены Wails; dlopen лишь находит их.
var gtk = struct {
	once        sync.Once
	ok          bool
	realize     func(uintptr)
	setVisible  func(uintptr, int32)
	surface     func(uintptr) uintptr
	display     func(uintptr) uintptr
	x11Display  func() uintptr
	isA         func(uintptr, uintptr) int32
	setUserTime func(uintptr, uint32)
}{}

func loadGTK() bool {
	gtk.once.Do(func() {
		lib, err := purego.Dlopen("libgtk-4.so.1", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			return
		}
		gobj, err := purego.Dlopen("libgobject-2.0.so.0", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			return
		}
		bind := func(fn any, h uintptr, name string) bool {
			p, err := purego.Dlsym(h, name)
			if err != nil {
				return false
			}
			purego.RegisterFunc(fn, p)
			return true
		}
		gtk.ok = bind(&gtk.realize, lib, "gtk_widget_realize") &&
			bind(&gtk.setVisible, lib, "gtk_widget_set_visible") &&
			bind(&gtk.surface, lib, "gtk_native_get_surface") &&
			bind(&gtk.display, lib, "gtk_widget_get_display") &&
			bind(&gtk.isA, gobj, "g_type_check_instance_is_a")
		// Функции X11 есть, только если GTK собран с X11; без них окно просто покажется.
		if !bind(&gtk.x11Display, lib, "gdk_x11_display_get_type") || !bind(&gtk.setUserTime, lib, "gdk_x11_surface_set_user_time") {
			gtk.x11Display, gtk.setUserTime = nil, nil
		}
	})
	return gtk.ok
}

func panelShow(w *application.WebviewWindow) {
	if !loadGTK() {
		w.Show()
		return
	}
	win := uintptr(w.NativeWindow())
	application.InvokeSync(func() {
		gtk.realize(win)
		if gtk.setUserTime != nil && gtk.isA(gtk.display(win), gtk.x11Display()) != 0 {
			if s := gtk.surface(win); s != 0 {
				gtk.setUserTime(s, 0)
			}
		}
		gtk.setVisible(win, 1)
	})
	w.SetAlwaysOnTop(true) // _NET_WM_STATE_ABOVE ставится на окно, которое уже на экране
}

func panelHide(w *application.WebviewWindow) { w.Hide() }
