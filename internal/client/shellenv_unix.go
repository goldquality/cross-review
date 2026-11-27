//go:build !windows

package client

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const pathMarker = "__CROSS_REVIEW_PATH__"

// LoginShellPath — PATH из логин-шелла участника ($SHELL -ilc). Пусто, если шелл не ответил за
// 10 секунд. К нему добавляются обычные места установки, если их там нет.
func LoginShellPath() string {
	sh := os.Getenv("SHELL")
	if sh == "" {
		sh = "/bin/sh"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, sh, "-ilc", `printf '\n`+pathMarker+`%s\n' "$PATH"`)
	cmd.Stdin = nil
	out, err := cmd.Output()
	path := ""
	if err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if p, ok := strings.CutPrefix(line, pathMarker); ok {
				path = strings.TrimSpace(p)
			}
		}
	}
	if path == "" {
		path = os.Getenv("PATH")
	}
	home, _ := os.UserHomeDir()
	have := map[string]bool{}
	for _, p := range filepath.SplitList(path) {
		have[p] = true
	}
	for _, extra := range []string{"/opt/homebrew/bin", "/usr/local/bin", filepath.Join(home, ".local/bin"), filepath.Join(home, "go/bin"), "/usr/bin", "/bin"} {
		if !have[extra] {
			if fi, err := os.Stat(extra); err == nil && fi.IsDir() {
				path += string(os.PathListSeparator) + extra
			}
		}
	}
	return path
}

func afterSeconds(n int) <-chan time.Time { return time.After(time.Duration(n) * time.Second) }
