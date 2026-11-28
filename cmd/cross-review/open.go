package main

import (
	"os/exec"
	"runtime"
)

// openEditor открывает файл настроек в текстовом редакторе.
func openEditor(path string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", "-t", path)
	case "windows":
		cmd = exec.Command("notepad.exe", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	startDetached(cmd)
}

// openURL открывает ссылку в браузере.
func openURL(u string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	startDetached(cmd)
}

// openFile открывает лог программой по умолчанию.
func openFile(path string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "windows":
		cmd = exec.Command("notepad.exe", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	startDetached(cmd)
}

// startDetached запускает программу и забирает её код выхода, когда она закончит: без Wait на
// unix от каждого клика оставался бы процесс-зомби.
func startDetached(cmd *exec.Cmd) {
	if cmd.Start() == nil {
		go cmd.Wait()
	}
}
