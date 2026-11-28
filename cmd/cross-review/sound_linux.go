package main

import "os/exec"

// playSound — звук темы рабочего стола, если есть canberra-gtk-play; нет — тихо.
func playSound() {
	if p, err := exec.LookPath("canberra-gtk-play"); err == nil {
		startDetached(exec.Command(p, "-i", "message-new-instant"))
	}
}
