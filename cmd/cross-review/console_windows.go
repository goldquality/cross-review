package main

import (
	"os"
	"syscall"
)

// attachConsole: бинарь собран как GUI (-H=windowsgui), чтобы трей не открывал окно консоли.
// Команды из терминала (join, status…) подключаются к консоли родителя, чтобы вывод был виден.
func attachConsole() {
	if len(os.Args) < 2 {
		return
	}
	attach := syscall.NewLazyDLL("kernel32.dll").NewProc("AttachConsole")
	const attachParent = ^uintptr(0) // ATTACH_PARENT_PROCESS = (DWORD)-1
	if r, _, _ := attach.Call(attachParent); r == 0 {
		return
	}
	if f, err := os.OpenFile("CONOUT$", os.O_RDWR, 0); err == nil {
		os.Stdout = f
		os.Stderr = f
	}
}
