package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// prepareDesktop — до запуска GTK, только для трея:
//   - На Wayland окно не может само встать в угол экрана и быть поверх остальных: Wails делает это
//     через X11. Если есть XWayland, GTK запускается через него.
//   - Песочнице WebKitGTK (bwrap) нужны пространства имён пользователя, а Ubuntu 24.04 запрещает их
//     через AppArmor — WebKit тогда падает при запуске. В этом случае песочница выключается:
//     страницы у клиента только свои, встроенные в бинарь, со строгим CSP, а строки брокера
//     попадают на них только текстом.
func prepareDesktop() {
	if os.Getenv("GDK_BACKEND") == "" && os.Getenv("WAYLAND_DISPLAY") != "" && os.Getenv("DISPLAY") != "" {
		os.Setenv("GDK_BACKEND", "x11")
	}
	if os.Getenv("WEBKIT_DISABLE_SANDBOX_THIS_IS_DANGEROUS") == "" && !userNamespacesWork() {
		os.Setenv("WEBKIT_DISABLE_SANDBOX_THIS_IS_DANGEROUS", "1")
		fmt.Fprintln(os.Stderr, "cross-review: система запрещает пространства имён пользователя, песочница WebKit выключена")
	}
}

// userNamespacesWork пробует то же, что песочница WebKit: bwrap с отдельным пространством имён.
func userNamespacesWork() bool {
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		return true // bwrap нет — WebKit решит сам
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, bwrap, "--ro-bind", "/", "/", "true").Run() == nil
}
