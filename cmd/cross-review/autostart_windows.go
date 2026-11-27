package main

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows/registry"
)

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

// install — запись в HKCU\…\Run: трей стартует при входе пользователя.
func install() error {
	exe, err := selfPath()
	if err != nil {
		return err
	}
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.SetStringValue("cross-review", `"`+exe+`"`); err != nil {
		return err
	}
	fmt.Println("Автозапуск включён.")
	return nil
}

func uninstall() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.DeleteValue("cross-review"); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	fmt.Println("Автозапуск выключен.")
	return nil
}

func autostartInstalled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue("cross-review")
	return err == nil
}
