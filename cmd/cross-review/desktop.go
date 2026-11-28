package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"github.com/goldquality/cross-review/internal/client"
	"github.com/goldquality/cross-review/internal/mrurl"
)

const appTitle = "cross-review"

// Страницы окна у трея и панели тостов: встроены в бинарь, чужих адресов не грузят (CSP в каждой).
//
//go:embed assets
var assets embed.FS

// desktop — клиент с интерфейсом: иконка трея, окно у неё и тосты справа вверху. Один интерфейс
// для macOS, Windows и Linux на Wails: HTML в системном WebView.
type desktop struct {
	dir     string
	store   *client.Store
	agent   *client.Agent
	app     *application.App
	tray    *application.SystemTray
	popover *application.WebviewWindow
	toasts  *toaster
	ctx     context.Context
	cancel  context.CancelFunc

	// refresh зовут агент, таймер и окно из разных горутин: icon и shownErr меняются под замком.
	mu       sync.Mutex
	icon     *trayState
	shownErr string // ошибка настроек, о которой уже сказали
}

func runTray(dir string, store *client.Store) error {
	prepareDesktop()
	// Второй запуск трея не стартует, а открывает окно первого: это делает Wails в newDesktop.
	d := newDesktop(dir, store, true)
	l, err := lockInstance(dir)
	if err != nil {
		return err // работает daemon
	}
	defer l.Unlock()
	d.agent = newAgent(dir, store, d)
	d.agent.OnChange = d.refresh
	d.agent.Ask = d.ask
	d.app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		go d.started()
	})
	return d.app.Run()
}

// newDesktop создаёт приложение, окно у трея, панель тостов и иконку. singleInstance — второй
// запуск с той же папкой клиента отдаёт управление первому и выходит.
func newDesktop(dir string, store *client.Store, singleInstance bool) *desktop {
	d := &desktop{dir: dir, store: store}
	d.ctx, d.cancel = context.WithCancel(context.Background())
	opts := application.Options{
		Name:        appTitle,
		Description: "Кросс-ревью MR",
		Services:    []application.Service{application.NewService(&UI{d: d})},
		Assets:      application.AssetOptions{Handler: application.BundledAssetFileServer(assets)},
		Mac:         application.MacOptions{ActivationPolicy: application.ActivationPolicyAccessory},
		Windows:     application.WindowsOptions{WebviewUserDataPath: filepath.Join(dir, "webview")},
		Linux:       application.LinuxOptions{ProgramName: appTitle},
		OnShutdown: func() {
			if d.agent != nil {
				d.agent.StopCurrent()
			}
			d.cancel()
		},
	}
	if singleInstance {
		opts.SingleInstance = &application.SingleInstanceOptions{
			UniqueID:               instanceID(dir),
			OnSecondInstanceLaunch: func(application.SecondInstanceData) { d.showWindow() },
		}
	}
	d.app = application.New(opts)

	d.popover = d.app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:            "main",
		Title:           appTitle,
		Width:           380,
		Height:          600,
		Frameless:       true,
		AlwaysOnTop:     true,
		Hidden:          true,
		DisableResize:   true,
		HideOnEscape:    true,
		HideOnFocusLost: true,
		URL:             "/index.html",
		Windows:         application.WindowsWindow{HiddenOnTaskbar: true},
	})
	d.popover.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		d.popover.Hide()
		e.Cancel()
	})
	d.toasts = newToaster(d.app)

	d.tray = d.app.SystemTray.New()
	d.setIcon(stateOffline)
	d.tray.SetTooltip(appTitle)
	menu := d.app.Menu.New()
	menu.Add("Открыть").OnClick(func(*application.Context) { d.showWindow() })
	menu.Add("Попросить ревью MR из буфера").OnClick(func(*application.Context) { go d.requestFromClipboard() })
	menu.AddSeparator()
	menu.Add("Выход").OnClick(func(*application.Context) { d.quit() })
	d.tray.SetMenu(menu)
	// Левый клик — окно у иконки, правый — меню.
	d.tray.AttachWindow(d.popover).WindowOffset(6)
	return d
}

// instanceID — один трей на папку клиента: с CROSS_REVIEW_HOME на машине живут два клиента.
func instanceID(dir string) string {
	h := sha256.Sum256([]byte(dir))
	return "com.github.goldquality.cross-review." + hex.EncodeToString(h[:8])
}

// started — приложение запущено: агент, обновление окна раз в 30 секунд, шаблон настроек.
func (d *desktop) started() {
	d.refresh()
	go d.agent.Run(d.ctx)
	go func() {
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-d.ctx.Done():
				return
			case <-tick.C:
				d.refresh()
			}
		}
	}()
	// Первый запуск: файла нет — создать шаблон и открыть его.
	if created, err := d.store.EnsureSettings(); err == nil && created {
		openEditor(d.store.Path())
		d.Notify(appTitle, "Заполни настройки и сохрани файл: "+d.store.Path())
	}
}

// Notify — уведомление тостом. Весь текст проходит client.Clean: в нём бывают строки брокера.
func (d *desktop) Notify(title, text string) {
	d.toasts.show(toastSpec{Title: client.Clean(title), Body: client.Clean(text), Primary: -1, Life: notifyLife})
}

// refresh — состояние агента изменилось: иконка, подсказка, окно.
func (d *desktop) refresh() {
	st := d.agent.State()
	d.mu.Lock()
	d.setIcon(iconState(st))
	newErr := st.SettingsErr != d.shownErr
	d.shownErr = st.SettingsErr
	d.mu.Unlock()
	d.tray.SetTooltip(appTitle + ": " + shorten(statusLine(st), 100))
	// Об ошибке в файле — тостом один раз: пока её не исправят, действуют прежние настройки.
	if newErr && st.SettingsErr != "" {
		d.Notify(appTitle, "Ошибка в настройках: "+st.SettingsErr)
	}
	d.app.Event.Emit("state")
}

func (d *desktop) setIcon(s trayState) {
	if d.icon != nil && *d.icon == s {
		return
	}
	d.icon = &s
	icon := trayIcon(s)
	if iconTemplate(s) {
		d.tray.SetTemplateIcon(icon)
	} else {
		d.tray.SetIcon(icon)
	}
}

// ask — пришло задание: тост с кнопками «Открыть MR», «Отклонить», «Принять» и полоской
// обратного отсчёта. «Открыть MR» открывает ссылку, тост остаётся. Истёк ctx — тост закрывается,
// ответ «нет».
func (d *desktop) ask(ctx context.Context, o client.JobOffer) bool {
	const open, decline, accept = 0, 1, 2
	l := offerText(o)
	id, answers := d.toasts.show(toastSpec{
		Title: l.who, Body: l.run, Footer: l.until,
		Buttons: []string{"Открыть MR", "Отклонить", "Принять"}, Primary: accept,
		Life: time.Until(o.Until), Sound: true,
	})
	defer d.toasts.close(id)
	for {
		select {
		case <-ctx.Done():
			return false
		case a := <-answers:
			switch a {
			case open:
				openURL(o.MR.String())
			case accept:
				return true
			case decline:
				return false
			}
		}
	}
}

// clipboardMR — каноничная ссылка на MR из буфера обмена или пусто.
func (d *desktop) clipboardMR() string {
	text, ok := d.app.Clipboard.Text()
	if !ok {
		return ""
	}
	mr, err := mrurl.NormalizeMR(strings.TrimSpace(text))
	if err != nil {
		return ""
	}
	return mr.String()
}

// requestFromClipboard — пункт меню трея: ссылка в буфере — сразу запрос, нет — окно с полем ссылки.
func (d *desktop) requestFromClipboard() {
	link := d.clipboardMR()
	if link == "" {
		d.showWindow()
		return
	}
	msg, err := d.request(link)
	if err != nil {
		msg = err.Error()
	}
	d.Notify(appTitle, msg)
}

// request просит ревью MR по ссылке и возвращает текст для человека.
func (d *desktop) request(link string) (string, error) {
	ctx, cancel := context.WithTimeout(d.ctx, 30*time.Second)
	defer cancel()
	mr, r, err := d.agent.RequestReview(ctx, link)
	if err != nil {
		return "", err
	}
	d.refresh()
	return client.DescribeRequest(mr, r), nil
}

func (d *desktop) showWindow() {
	if runtime.GOOS == "linux" {
		// У трея на Linux нет координат иконки: окно встаёт в правый верхний угол.
		if s := d.app.Screen.GetPrimary(); s != nil {
			w, _ := d.popover.Size()
			d.popover.SetPosition(s.WorkArea.X+s.WorkArea.Width-w-toastMargin, s.WorkArea.Y+toastMargin)
		}
		d.popover.Show().Focus()
		return
	}
	d.tray.ShowWindow()
}

func (d *desktop) quit() {
	if d.agent != nil {
		d.agent.StopCurrent()
	}
	d.app.Quit()
}
