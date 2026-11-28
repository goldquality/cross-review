package main

import (
	"fmt"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// toastSpec — что показать в тосте. Строки брокера сюда попадают как есть: страница кладёт их
// только в textContent.
type toastSpec struct {
	Title   string
	Body    string
	Footer  string
	Buttons []string
	Primary int // номер главной кнопки
	Seconds int // без кнопок — сколько висеть; с кнопками — длина полоски обратного отсчёта
}

const (
	toastWidth   = 360
	toastMargin  = 12
	toastSpacing = 8
	toastGuessH  = 96 // высота до того, как страница измерит себя
)

type toast struct {
	id      int64
	spec    toastSpec
	win     *application.WebviewWindow
	height  int
	shown   bool
	answers chan int
	x, y    int
}

// toaster ставит тосты столбиком от правого верхнего угла рабочей области главного экрана.
// Каждый тост — своё окно без рамки поверх остальных; на macOS это панель, которая не делает
// приложение активным, на Windows — окно с WS_EX_NOACTIVATE.
type toaster struct {
	app *application.App

	mu    sync.Mutex
	next  int64
	list  []*toast
	focus []string // заметки «тост N получил фокус» для отчёта
}

func newToaster(app *application.App) *toaster { return &toaster{app: app} }

// show показывает тост; в канал приходят номера нажатых кнопок. Окно появляется, когда страница
// измерит свою высоту, — или через 2 секунды, если не измерит.
func (t *toaster) show(s toastSpec) (int64, <-chan int) {
	t.mu.Lock()
	t.next++
	ts := &toast{id: t.next, spec: s, height: toastGuessH, answers: make(chan int, 4)}
	t.list = append(t.list, ts)
	x, y := t.slot(ts)
	ts.x, ts.y = x, y
	t.mu.Unlock()

	ts.win = t.app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:            fmt.Sprintf("toast-%d", ts.id),
		Title:           "cross-review",
		Width:           toastWidth,
		Height:          toastGuessH,
		Frameless:       true,
		AlwaysOnTop:     true,
		Hidden:          true,
		DisableResize:   true,
		InitialPosition: application.WindowXY,
		X:               x,
		Y:               y,
		URL:             fmt.Sprintf("/toast.html?id=%d", ts.id),
		Mac: application.MacWindow{
			WindowClass:      application.MacWindowClassPanel,
			PanelPreferences: application.MacPanelPreferences{NonActivating: true},
			WindowLevel:      application.MacWindowLevelFloating,
			CollectionBehavior: application.MacWindowCollectionBehaviorCanJoinAllSpaces |
				application.MacWindowCollectionBehaviorFullScreenAuxiliary |
				application.MacWindowCollectionBehaviorStationary |
				application.MacWindowCollectionBehaviorIgnoresCycle,
		},
		Windows: application.WindowsWindow{HiddenOnTaskbar: true, ExStyle: toastExStyle()},
	})
	if len(s.Buttons) == 0 && s.Seconds > 0 {
		time.AfterFunc(time.Duration(s.Seconds)*time.Second, func() { t.close(ts.id) })
	}
	time.AfterFunc(2*time.Second, func() { t.reveal(ts.id) })
	return ts.id, ts.answers
}

// slot — где стоит тост ts при нынешних высотах тех, что выше него. Под замком.
func (t *toaster) slot(ts *toast) (int, int) {
	wa := application.Rect{X: 0, Y: 0, Width: 1440, Height: 900}
	if s := t.app.Screen.GetPrimary(); s != nil {
		wa = s.WorkArea
	}
	x := wa.X + wa.Width - toastWidth - toastMargin
	y := wa.Y + toastMargin
	for _, o := range t.list {
		if o == ts {
			break
		}
		y += o.height + toastSpacing
	}
	return x, y
}

func (t *toaster) find(id int64) *toast {
	for _, ts := range t.list {
		if ts.id == id {
			return ts
		}
	}
	return nil
}

func (t *toaster) spec(id int64) (toastSpec, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if ts := t.find(id); ts != nil {
		return ts.spec, nil
	}
	return toastSpec{}, fmt.Errorf("тоста %d нет", id)
}

func (t *toaster) action(id int64, a int) {
	t.mu.Lock()
	ts := t.find(id)
	t.mu.Unlock()
	if ts == nil {
		return
	}
	if len(ts.spec.Buttons) == 0 {
		t.close(id)
		return
	}
	select {
	case ts.answers <- a:
	default:
	}
}

func (t *toaster) resize(id int64, h int) {
	h = min(max(h, 48), 400)
	t.mu.Lock()
	ts := t.find(id)
	if ts != nil {
		ts.height = h
	}
	t.mu.Unlock()
	if ts == nil {
		return
	}
	ts.win.SetSize(toastWidth, h)
	t.layout()
	t.reveal(id)
}

// reveal показывает окно тоста, не забирая фокус, и через полсекунды записывает, не стал ли тост
// всё-таки активным окном.
func (t *toaster) reveal(id int64) {
	t.mu.Lock()
	ts := t.find(id)
	if ts == nil || ts.shown {
		t.mu.Unlock()
		return
	}
	ts.shown = true
	t.mu.Unlock()
	beforeTitle, beforeOurs, known := foreground()
	showWithoutFocus(ts.win)
	time.AfterFunc(500*time.Millisecond, func() {
		note := ""
		if known {
			// Windows: забрал ли тост фокус у чужого окна. Если активным уже было наше окно,
			// Windows разрешает процессу активировать свои окна — это не кража.
			if _, ours, _ := foreground(); ours && !beforeOurs {
				note = fmt.Sprintf("тост %d забрал фокус у «%s»", id, beforeTitle)
			}
		} else if ts.win.IsFocused() {
			note = fmt.Sprintf("тост %d стал активным окном", id)
		}
		if note != "" {
			t.mu.Lock()
			t.focus = append(t.focus, note)
			t.mu.Unlock()
		}
	})
}

func (t *toaster) close(id int64) {
	t.mu.Lock()
	ts := t.find(id)
	if ts != nil {
		for i, o := range t.list {
			if o == ts {
				t.list = append(t.list[:i], t.list[i+1:]...)
				break
			}
		}
	}
	t.mu.Unlock()
	if ts == nil {
		return
	}
	ts.win.Close()
	t.layout()
}

// layout переставляет тосты столбиком после того, как один закрылся или поменял высоту.
func (t *toaster) layout() {
	t.mu.Lock()
	type move struct {
		w    *application.WebviewWindow
		x, y int
	}
	var moves []move
	for _, ts := range t.list {
		x, y := t.slot(ts)
		if x != ts.x || y != ts.y {
			ts.x, ts.y = x, y
			moves = append(moves, move{ts.win, x, y})
		}
	}
	t.mu.Unlock()
	for _, m := range moves {
		m.w.SetPosition(m.x, m.y)
	}
}

// snapshot — где стоят тосты, для отчёта.
func (t *toaster) snapshot() []map[string]any {
	t.mu.Lock()
	list := append([]*toast(nil), t.list...)
	t.mu.Unlock()
	var out []map[string]any
	for _, ts := range list {
		x, y := ts.win.Position()
		w, h := ts.win.Size()
		out = append(out, map[string]any{"id": ts.id, "x": x, "y": y, "w": w, "h": h, "want": []int{ts.x, ts.y}, "focused": ts.win.IsFocused(), "visible": ts.win.IsVisible()})
	}
	return out
}

func (t *toaster) focusLog() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.focus...)
}
