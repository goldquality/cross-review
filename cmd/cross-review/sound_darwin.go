package main

import "os/exec"

// playSound — звук вопроса о задании, тот же, что был у тостов на Cocoa.
func playSound() { startDetached(exec.Command("afplay", "/System/Library/Sounds/Glass.aiff")) }
