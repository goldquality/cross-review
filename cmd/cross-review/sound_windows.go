package main

import "syscall"

var messageBeep = syscall.NewLazyDLL("user32.dll").NewProc("MessageBeep")

// playSound — системный звук уведомления (MB_ICONASTERISK).
func playSound() { messageBeep.Call(0x40) }
