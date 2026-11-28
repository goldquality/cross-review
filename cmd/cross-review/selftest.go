package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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
	d.toasts.trace = traceAdd
	go watchForeground()
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
			// Код выхода — сразу: на macOS Quit() завершает процесс, не возвращаясь из Run().
			if problems := selftestReport(d); len(problems) > 0 {
				os.Exit(1)
			}
			d.app.Quit()
		}()
	})
	return d.app.Run()
}

// trace — лента событий selftest-ui: показ и сокрытие панели, её мерки и смены активного окна.
var trace = struct {
	sync.Mutex
	start time.Time
	lines []string
}{start: time.Now()}

func traceAdd(format string, args ...any) {
	trace.Lock()
	defer trace.Unlock()
	trace.lines = append(trace.lines, fmt.Sprintf("%5.2fs ", time.Since(trace.start).Seconds())+fmt.Sprintf(format, args...))
}

// watchForeground записывает в ленту каждую смену активного окна (только Windows).
func watchForeground() {
	if _, _, ok := foreground(); !ok {
		return
	}
	last := ""
	for range time.Tick(250 * time.Millisecond) {
		title, ours, _ := foreground()
		cur := fmt.Sprintf("активно «%s», наше: %v", title, ours)
		if cur != last {
			last = cur
			traceAdd("%s", cur)
		}
	}
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
	// Уведомление к отчёту уже закрылось само; вопрос о задании висит до конца.
	if n != 1 {
		p = append(p, fmt.Sprintf("тостов %d, а должен остаться один — вопрос о задании", n))
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
	trace.Lock()
	r["trace"] = append([]string(nil), trace.lines...)
	trace.Unlock()
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
