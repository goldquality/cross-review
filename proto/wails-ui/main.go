// Прототип интерфейса cross-review на Wails v3 (шаг 0 плана): окно у иконки трея и тосты справа
// вверху, одинаковые на macOS, Windows и Linux. В клиент не входит: отдельный модуль, чтобы
// `go test ./...` корня не тянул Wails и GTK.
//
// Что проверяет — в README.md рядом.
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"runtime"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

//go:embed assets
var assets embed.FS

func main() {
	demo := flag.Bool("demo", false, "сразу показать окно и тосты, без кликов (для CI и скриншотов)")
	exitAfter := flag.Duration("exit", 0, "выйти через это время и напечатать отчёт (для CI)")
	hostileNames := flag.Bool("hostile", false, "имя участника с HTML внутри, как от враждебного брокера (для CI)")
	showPopover := flag.Bool("popover", true, "в демо показать окно с данными; false — только тосты, для честной проверки фокуса")
	delay := flag.Duration("delay", time.Second, "в демо — пауза перед первым тостом")
	flag.Parse()
	if *hostileNames {
		stranger = hostile
	}
	prepareLinux()

	ui := newUI()
	app := application.New(application.Options{
		Name:     "cross-review-proto",
		Services: []application.Service{application.NewService(ui)},
		Assets:   application.AssetOptions{Handler: application.BundledAssetFileServer(assets)},
		Mac:      application.MacOptions{ActivationPolicy: application.ActivationPolicyAccessory},
	})
	ui.app = app
	ui.toasts = newToaster(app)

	popover := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:            "popover",
		Title:           "cross-review",
		Width:           380,
		Height:          560,
		Frameless:       true,
		AlwaysOnTop:     true,
		Hidden:          true,
		DisableResize:   true,
		HideOnEscape:    true,
		HideOnFocusLost: !*demo,
		URL:             "/index.html",
		Windows:         application.WindowsWindow{HiddenOnTaskbar: true},
	})
	popover.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		popover.Hide()
		e.Cancel()
	})
	ui.popover = popover

	tray := app.SystemTray.New()
	icon := trayIcon()
	if runtime.GOOS == "darwin" {
		tray.SetTemplateIcon(icon)
	} else {
		tray.SetIcon(icon)
	}
	tray.SetTooltip("cross-review (прототип)")
	menu := app.Menu.New()
	menu.Add("Показать уведомление").OnClick(func(*application.Context) { ui.demoNotify() })
	menu.Add("Показать вопрос о задании").OnClick(func(*application.Context) { go ui.demoOffer() })
	menu.AddSeparator()
	menu.Add("Выход").OnClick(func(*application.Context) { app.Quit() })
	tray.SetMenu(menu)
	if !*demo {
		tray.AttachWindow(popover).WindowOffset(6)
	}

	if *demo {
		app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
			go func() {
				// В демо окно не привязано к трею: та привязка прячет его, едва фокус уйдёт. Ставим
				// его левее столбика тостов, чтобы на скриншоте было видно и то и другое. Показ окна
				// делает прототип активным приложением, а своему активному процессу Windows
				// разрешает активировать любые его окна, — поэтому для проверки фокуса -popover=false.
				if *showPopover {
					time.Sleep(time.Second)
					if s := app.Screen.GetPrimary(); s != nil {
						wa := s.WorkArea
						popover.SetPosition(wa.X+wa.Width-toastWidth-2*toastMargin-380, wa.Y+toastMargin)
					}
					popover.Show()
				}
				time.Sleep(*delay)
				ui.demoNotify()
				time.Sleep(500 * time.Millisecond)
				ui.demoOffer()
			}()
		})
	}
	exitCode := 0
	if *exitAfter > 0 {
		go func() {
			time.Sleep(*exitAfter)
			if problems := report(app, ui, *demo); len(problems) > 0 {
				exitCode = 1
			}
			app.Quit()
		}()
	}

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
	os.Exit(exitCode)
}

// report печатает, где оказались окна и забирали ли тосты фокус, и возвращает найденные проблемы:
// по ним CI судит о раскладке на каждой ОС без глаз.
func report(app *application.App, ui *UI, demo bool) []string {
	r := map[string]any{"os": runtime.GOOS, "arch": runtime.GOARCH}
	if s := app.Screen.GetPrimary(); s != nil {
		r["screen"] = map[string]any{"bounds": s.Bounds, "workArea": s.WorkArea, "scale": s.ScaleFactor}
	}
	r["toasts"] = ui.toasts.snapshot()
	r["focus"] = ui.toasts.focusLog()
	r["calls"] = ui.callCounts()
	r["injectedElements"] = ui.injectedCount()
	r["popover"] = map[string]any{"visible": ui.popover.IsVisible(), "focused": ui.popover.IsFocused()}
	if title, ours, ok := foreground(); ok {
		r["foreground"] = map[string]any{"title": title, "ours": ours}
	}
	var problems []string
	if demo {
		problems = check(ui)
	}
	r["problems"] = problems
	b, _ := json.MarshalIndent(r, "", "  ")
	fmt.Fprintln(os.Stdout, "REPORT "+string(b))
	return problems
}

// check — что должно быть верно после демо: мост JS → Go жив, строки брокера остались текстом,
// тосты стоят там, куда их поставили, и не забрали фокус.
func check(ui *UI) []string {
	var p []string
	calls := ui.callCounts()
	for _, m := range []string{"State", "Toast", "ToastResize", "RenderCheck"} {
		if calls[m] == 0 {
			p = append(p, "страница не вызвала "+m)
		}
	}
	if n := ui.injectedCount(); n > 0 {
		p = append(p, fmt.Sprintf("строка брокера стала HTML: %d элементов <img>", n))
	}
	p = append(p, ui.toasts.focusLog()...)
	toasts := ui.toasts.snapshot()
	if len(toasts) == 0 {
		p = append(p, "к концу демо не осталось ни одного тоста")
	}
	for _, t := range toasts {
		want := t["want"].([]int)
		x, y := t["x"].(int), t["y"].(int)
		if abs(x-want[0]) > 2 || abs(y-want[1]) > 2 {
			p = append(p, fmt.Sprintf("тост %v стоит в (%d,%d), а не в (%d,%d)", t["id"], x, y, want[0], want[1]))
		}
		if t["focused"].(bool) {
			p = append(p, fmt.Sprintf("тост %v — активное окно", t["id"]))
		}
	}
	return p
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
