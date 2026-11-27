package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func desktopPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "autostart", "cross-review.desktop"), nil
}

// install — XDG autostart: трей стартует при входе в графическую сессию.
func install() error {
	exe, err := selfPath()
	if err != nil {
		return err
	}
	p, err := desktopPath()
	if err != nil {
		return err
	}
	quoted := `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "`", "\\`", "$", `\$`).Replace(exe) + `"`
	entry := "[Desktop Entry]\nType=Application\nName=cross-review\nComment=Кросс-ревью MR\nExec=" + quoted + "\nTerminal=false\nX-GNOME-Autostart-enabled=true\n"
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte(entry), 0o644); err != nil {
		return err
	}
	fmt.Println("Автозапуск включён:", p)
	return nil
}

func uninstall() error {
	p, err := desktopPath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Println("Автозапуск выключен.")
	return nil
}

func autostartInstalled() bool {
	p, err := desktopPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}
