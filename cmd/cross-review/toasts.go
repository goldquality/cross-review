package main

import (
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// toastSpec — что показать в тосте. Текст от брокера к этому моменту уже прошёл client.Clean.
type toastSpec struct {
	Title, Body, Footer string
	Buttons             []string
	Primary             int           // номер главной кнопки, -1 — нет
	Life                time.Duration // без кнопок — сколько висеть; с кнопками — полоска отсчёта
	Sound               bool
}

// ToastView — тост для страницы панели.
type ToastView struct {
	ID      int64
	Title   string
	Body    string
	Footer  string
	Buttons []string
	Primary int
	Total   int64 // мс, длина полоски отсчёта; 0 — без полоски
	Left    int64 // мс до конца отсчёта
}

const (
	toastWidth  = 360
	toastMargin = 12
	notifyLife  = 6 * time.Second
)

type toast struct {
	id      int64
	spec    toastSpec
	start   time.Time
	answers chan int
	hovered bool
}

// toaster — тосты в правом верхнем углу главного экрана. Все тосты — строки одной панели: окно без
// рамки поверх остальных создаётся при запуске клиента и дальше только показывается и прячется.
// Новое окно WebView2 при создании забирает фокус, а показ готового окна на каждой ОС умеет
// обходиться без этого (panel_*.go). Так тост не прерывает набор текста в другом приложении.
type toaster struct {
	app *application.App
	win *application.WebviewWindow

	mu      sync.Mutex
	next    int64
	list    []*toast
	height  int // высота стопки, которую измерила страница
	visible bool
	calls   map[string]int       // какие методы звала страница панели: для selftest-ui
	trace   func(string, ...any) // лента событий для selftest-ui; nil — не вести
}

func (t *toaster) note(format string, args ...any) {
	if t.trace != nil {
		t.trace(format, args...)
	}
}

func newToaster(app *application.App) *toaster {
	t := &toaster{app: app, calls: map[string]int{}}
	opts := application.WebviewWindowOptions{
		Name:            "toasts",
		Title:           appTitle,
		Width:           toastWidth,
		Height:          100,
		Frameless:       true,
		AlwaysOnTop:     true,
		Hidden:          true,
		DisableResize:   true,
		InitialPosition: application.WindowXY,
		URL:             "/panel.html",
	}
	opts.X, opts.Y = t.corner()
	panelOptions(&opts)
	t.win = app.Window.NewWithOptions(opts)
	return t
}

// corner — левый верхний угол панели: правый верхний угол рабочей области главного экрана.
func (t *toaster) corner() (int, int) {
	s := t.app.Screen.GetPrimary()
	if s == nil {
		return 0, 0
	}
	wa := s.WorkArea
	return wa.X + wa.Width - toastWidth - toastMargin, wa.Y + toastMargin
}

// warmUp готовит панель к показу без фокуса; зовётся один раз после запуска приложения.
func (t *toaster) warmUp() { panelWarmUp(t.win) }

// show показывает тост; в канал приходят номера нажатых кнопок.
func (t *toaster) show(s toastSpec) (int64, <-chan int) {
	t.mu.Lock()
	t.next++
	ts := &toast{id: t.next, spec: s, start: time.Now(), answers: make(chan int, 8)}
	t.list = append(t.list, ts)
	t.mu.Unlock()
	if len(s.Buttons) == 0 && s.Life > 0 {
		t.autoClose(ts.id, s.Life)
	}
	if s.Sound {
		go playSound()
	}
	t.changed()
	return ts.id, ts.answers
}

// autoClose закрывает тост без кнопок через d; пока над ним мышь — ждёт.
func (t *toaster) autoClose(id int64, d time.Duration) {
	time.AfterFunc(d, func() {
		t.mu.Lock()
		ts := t.find(id)
		hovered := ts != nil && ts.hovered
		t.mu.Unlock()
		if hovered {
			t.autoClose(id, time.Second)
			return
		}
		t.close(id)
	})
}

func (t *toaster) find(id int64) *toast {
	for _, ts := range t.list {
		if ts.id == id {
			return ts
		}
	}
	return nil
}

// close убирает тост; последний закрытый прячет панель.
func (t *toaster) close(id int64) {
	t.mu.Lock()
	for i, ts := range t.list {
		if ts.id == id {
			t.list = append(t.list[:i], t.list[i+1:]...)
			break
		}
	}
	empty := len(t.list) == 0
	t.mu.Unlock()
	if empty {
		t.hide()
	}
	t.changed()
}

// changed просит страницу панели перечитать тосты.
func (t *toaster) changed() { t.app.Event.Emit("toasts") }

func (t *toaster) count(method string) {
	t.mu.Lock()
	t.calls[method]++
	t.mu.Unlock()
}

// views — тосты для страницы панели.
func (t *toaster) views() []ToastView {
	t.count("Toasts")
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]ToastView, 0, len(t.list))
	for _, ts := range t.list {
		v := ToastView{ID: ts.id, Title: ts.spec.Title, Body: ts.spec.Body, Footer: ts.spec.Footer, Buttons: ts.spec.Buttons, Primary: ts.spec.Primary}
		if len(ts.spec.Buttons) > 0 && ts.spec.Life > 0 {
			v.Total = ts.spec.Life.Milliseconds()
			v.Left = max(0, (ts.spec.Life - time.Since(ts.start)).Milliseconds())
		}
		out = append(out, v)
	}
	return out
}

// action — нажата кнопка тоста; -1 — клик по тосту без кнопок, он закрывает тост.
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
	if a < 0 || a >= len(ts.spec.Buttons) {
		return
	}
	select {
	case ts.answers <- a:
	default:
	}
}

func (t *toaster) hover(id int64, on bool) {
	t.mu.Lock()
	if ts := t.find(id); ts != nil {
		ts.hovered = on
	}
	t.mu.Unlock()
}

// resize — страница измерила стопку тостов: панель встаёт в угол нужной высоты и показывается.
// h = 0 — страница не может себя измерить: скрытое окно GTK не имеет ширины. Тогда панель
// показывается с прежней или примерной высотой, а страница перемерит себя уже на экране.
func (t *toaster) resize(h int) {
	t.count("PanelResize")
	t.note("PanelResize(%d)", h)
	t.mu.Lock()
	if h <= 0 {
		h = t.height
		if h <= 0 {
			h = 120
		}
	}
	h = min(h, 2000)
	t.height = h
	empty := len(t.list) == 0
	show := !empty && !t.visible
	if show {
		t.visible = true
	}
	t.mu.Unlock()
	if empty {
		t.hide()
		return
	}
	t.place(h)
	if show {
		t.note("показ панели")
		panelShow(t.win)
		// GNOME ставит новое окно куда решит сам и не смотрит на начальные координаты: место
		// задаём ещё раз, когда окно уже на экране.
		t.place(h)
	}
}

func (t *toaster) place(h int) {
	x, y := t.corner()
	t.win.SetSize(toastWidth, h)
	t.win.SetPosition(x, y)
}

func (t *toaster) hide() {
	t.mu.Lock()
	was := t.visible
	t.visible = false
	t.mu.Unlock()
	if was {
		t.note("панель спрятана")
		panelHide(t.win)
	}
}
