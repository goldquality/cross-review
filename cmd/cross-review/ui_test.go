package main

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Все экспортированные методы UI вызываются из JS. Новый метод — новая дверь из страницы в Go:
// он должен быть намерением (без команд, путей и адресов) и попасть в этот список осознанно.
func TestUIMethods(t *testing.T) {
	want := []string{
		"ClipboardMR", "HideWindow", "OpenLastLog", "OpenReleases", "OpenSettings", "PanelResize",
		"Pause", "Quit", "Request", "Requests", "SetAutostart", "StopCurrent", "ToastAction",
		"ToastHover", "Toasts", "View",
	}
	typ := reflect.TypeOf(&UI{})
	var got []string
	for i := range typ.NumMethod() {
		m := typ.Method(i)
		got = append(got, m.Name)
		// Строку принимает только Request: её разбирает mrurl так же строго, как ссылку от брокера.
		for j := 1; j < m.Type.NumIn(); j++ {
			if m.Type.In(j).Kind() == reflect.String && m.Name != "Request" {
				t.Errorf("%s принимает строку: страница не должна передавать команды, пути и адреса", m.Name)
			}
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("методы UI:\n  есть   %s\n  ждали  %s", strings.Join(got, " "), strings.Join(want, " "))
	}
}
