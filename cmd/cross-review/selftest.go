package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"github.com/goldquality/cross-review/internal/client"
	"github.com/goldquality/cross-review/internal/mrurl"
)

// selftestUI — `cross-review selftest-ui`: проверка окон на этой машине, без брокера и без своих
// настроек (папка клиента — временная). Показывает уведомление и вопрос о задании, через -exit
// печатает отчёт: где встала панель тостов, не забрала ли она фокус, жив ли мост JS ↔ Go.
// Код выхода 1 — что-то не так. Нужна CI на трёх ОС и участнику, у которого не видно тостов.
func selftestUI(args []string) error {
	fs := flag.NewFlagSet("selftest-ui", flag.ContinueOnError)
	delay := fs.Duration("delay", 2*time.Second, "пауза перед первым тостом")
	exit := fs.Duration("exit", 12*time.Second, "когда напечатать отчёт и выйти")
	if err := fs.Parse(args); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "cross-review-selftest-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	store, err := client.OpenStore(tmp, filepath.Join(tmp, client.SettingsFile))
	if err != nil {
		return err
	}
	prepareDesktop()
	d := newDesktop(tmp, store, false)
	d.agent = newAgent(tmp, store, d)
	var problems []string
	d.app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		go func() {
			d.toasts.warmUp()
			time.Sleep(*delay)
			d.Notify(appTitle, "Проверка окон: это уведомление закроется само")
			mr, _ := mrurl.ParseMR("https://gitlab.example.com/acme/shop/-/merge_requests/9")
			ctx, cancel := context.WithTimeout(d.ctx, *exit)
			defer cancel()
			// Имя с разметкой — как от враждебного брокера: на панели оно должно остаться текстом.
			go d.ask(ctx, client.JobOffer{MR: mr, Requester: `<img src=x onerror="alert(1)">Вася`, Command: "./dev review <LINK>", Workdir: tmp, Until: time.Now().Add(*exit)})
			time.Sleep(*exit - *delay - time.Second)
			problems = selftestReport(d)
			d.app.Quit()
		}()
	})
	if err := d.app.Run(); err != nil {
		return err
	}
	if len(problems) > 0 {
		os.Exit(1)
	}
	return nil
}

// selftestReport печатает отчёт и возвращает найденные проблемы.
func selftestReport(d *desktop) []string {
	t := d.toasts
	t.mu.Lock()
	calls := map[string]int{}
	for k, v := range t.calls {
		calls[k] = v
	}
	n, height, visible := len(t.list), t.height, t.visible
	t.mu.Unlock()
	x, y := t.win.Position()
	w, h := t.win.Size()
	wantX, wantY := t.corner()
	r := map[string]any{
		"calls":  calls,
		"toasts": n,
		"panel":  map[string]any{"x": x, "y": y, "w": w, "h": h, "want": []int{wantX, wantY}, "measured": height, "visible": visible, "focused": t.win.IsFocused()},
	}
	if s := d.app.Screen.GetPrimary(); s != nil {
		r["screen"] = map[string]any{"bounds": s.Bounds, "workArea": s.WorkArea, "scale": s.ScaleFactor}
	}
	title, ours, known := foreground()
	if known {
		r["foreground"] = map[string]any{"title": title, "ours": ours}
	}

	var p []string
	for _, m := range []string{"Toasts", "PanelResize"} {
		if calls[m] == 0 {
			p = append(p, "панель тостов не вызвала "+m)
		}
	}
	if n != 2 {
		p = append(p, fmt.Sprintf("тостов %d, а должно быть 2", n))
	}
	if !visible {
		p = append(p, "панель тостов не показана")
	}
	if abs(x-wantX) > 2 || abs(y-wantY) > 2 {
		p = append(p, fmt.Sprintf("панель в (%d,%d), а не в правом верхнем углу (%d,%d)", x, y, wantX, wantY))
	}
	if h < 100 {
		p = append(p, fmt.Sprintf("панель высотой %d: тосты не поместились", h))
	}
	if known && ours {
		p = append(p, "активно окно клиента: тост забрал фокус у «"+title+"»? (проверять, запустив другое окно после клиента)")
	} else if !known && t.win.IsFocused() {
		p = append(p, "панель тостов стала активным окном")
	}
	r["problems"] = p
	b, _ := json.MarshalIndent(r, "", "  ")
	fmt.Println("REPORT " + string(b))
	return p
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
