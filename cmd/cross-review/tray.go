package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"fyne.io/systray"
	"github.com/atotto/clipboard"
	"github.com/ncruces/zenity"

	"github.com/goldquality/cross-review/internal/client"
	"github.com/goldquality/cross-review/internal/mrurl"
)

const (
	appTitle     = "cross-review"
	maxProjSlots = 12
	maxTeamSlots = 30
	topSlots     = 5
)

type trayNotifier struct{}

// systemNotify — системное уведомление; переменная, чтобы тест видел, что уходит в zenity.
var systemNotify = func(title, text string) { zenity.Notify(text, zenity.Title(title)) }

// Notify — системное уведомление. Весь текст проходит client.Clean: в нём бывают строки брокера, а
// на Windows zenity вставляет текст уведомления в скрипт PowerShell.
func (trayNotifier) Notify(title, text string) {
	systemNotify(client.Clean(title), client.Clean(text))
}

type tray struct {
	dir    string
	store  *client.Store
	agent  *client.Agent
	ctx    context.Context
	dialog sync.Mutex // одно окно за раз
	// refresh зовут агент, таймер и окна из разных горутин: idle и shownErr меняются под замком.
	refreshMu sync.Mutex

	status, update            *systray.MenuItem
	current, stop, lastLog    *systray.MenuItem
	request, requests         *systray.MenuItem
	online, top               *systray.MenuItem
	onlineSlots, topSlots     []*systray.MenuItem
	projects                  *systray.MenuItem
	projSlots                 []*systray.MenuItem
	pause, unpause            *systray.MenuItem
	pauseItems                []*systray.MenuItem
	settings, autostart, quit *systray.MenuItem
	idle                      *bool
	shownErr                  string // ошибка настроек, о которой уже сказали
}

func runTray(dir string, store *client.Store) error {
	l, err := lockInstance(dir)
	if err != nil {
		zenity.Info("cross-review уже запущен: иконка в трее.", zenity.Title(appTitle))
		return nil
	}
	defer l.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	t := &tray{dir: dir, store: store, ctx: ctx}
	t.agent = newAgent(dir, store, trayNotifier{})
	t.agent.OnChange = t.refresh
	t.agent.Ask = t.ask
	systray.Run(t.onReady, cancel)
	return nil
}

func (t *tray) onReady() {
	t.setIcon(true)
	systray.SetTooltip(appTitle)

	t.status = systray.AddMenuItem("…", "")
	t.status.Disable()
	t.update = systray.AddMenuItem("", "Открыть страницу релизов")
	t.update.Hide()
	systray.AddSeparator()
	t.request = systray.AddMenuItem("Попросить ревью MR из буфера", "Ссылка на MR из буфера обмена; запрос стоит 1 балл")
	t.requests = systray.AddMenuItem("Мои запросы", "")
	systray.AddSeparator()
	t.online = systray.AddMenuItem("Онлайн", "Кто из команды сейчас с клиентом")
	t.onlineSlots = addSlots(t.online, maxTeamSlots)
	t.top = systray.AddMenuItem("Топ по баллам", "")
	t.topSlots = addSlots(t.top, topSlots)
	systray.AddSeparator()
	t.current = systray.AddMenuItem("Заданий нет", "")
	t.current.Disable()
	t.stop = systray.AddMenuItem("Остановить ревью", "Задание уйдёт другому участнику")
	t.lastLog = systray.AddMenuItem("Открыть лог последнего ревью", "")
	systray.AddSeparator()
	t.projects = systray.AddMenuItem("Проекты", "Список — в файле настроек")
	t.projSlots = addSlots(t.projects, maxProjSlots)
	t.pause = systray.AddMenuItem("Пауза", "На паузе задания не приходят; просить ревью можно")
	for _, h := range PauseHours {
		t.pauseItems = append(t.pauseItems, t.pause.AddSubMenuItem(fmt.Sprintf("%d ч", h), ""))
	}
	t.unpause = t.pause.AddSubMenuItem("Снять паузу", "")
	systray.AddSeparator()
	t.settings = systray.AddMenuItem("Настройки…", "Открыть файл настроек в редакторе")
	t.autostart = systray.AddMenuItemCheckbox("Запускать при входе в систему", "", autostartInstalled())
	t.quit = systray.AddMenuItem("Выход", "")

	t.refresh()
	go t.agent.Run(t.ctx)
	go t.loop()
	go func() {
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-t.ctx.Done():
				return
			case <-tick.C:
				t.refresh()
			}
		}
	}()
	// Первый запуск: файла нет — создать шаблон и открыть его.
	if created, err := t.store.EnsureSettings(); err == nil && created {
		openEditor(t.store.Path())
		trayNotifier{}.Notify(appTitle, "Заполни настройки и сохрани файл: "+t.store.Path())
	}
}

// addSlots — n скрытых строк подменю: systray не умеет удалять пункты, поэтому строки заводятся
// заранее и показываются по мере надобности.
func addSlots(parent *systray.MenuItem, n int) []*systray.MenuItem {
	slots := make([]*systray.MenuItem, 0, n)
	for i := 0; i < n; i++ {
		s := parent.AddSubMenuItem("", "")
		s.Disable()
		s.Hide()
		slots = append(slots, s)
	}
	return slots
}

// fillSlots показывает строки lines в слотах, лишние прячет. Не влезло — последняя строка «…ещё N».
func fillSlots(slots []*systray.MenuItem, lines []string) {
	if len(lines) > len(slots) {
		rest := len(lines) - len(slots) + 1
		lines = append(lines[:len(slots)-1:len(slots)-1], fmt.Sprintf("…ещё %d", rest))
	}
	for i, s := range slots {
		if i < len(lines) {
			s.SetTitle(lines[i])
			s.Show()
		} else {
			s.Hide()
		}
	}
}

var teammateState = map[string]string{"free": "свободен", "busy": "ревьюит", "paused": "пауза"}

// teamLines — строки подменю «Онлайн» и «Топ по баллам»; себя участник видит с пометкой «(ты)».
func teamLines(team client.Team, me string) (online, top []string) {
	who := func(name string) string {
		if strings.EqualFold(name, me) {
			return name + " (ты)"
		}
		return name
	}
	for _, m := range team.Online {
		line := who(m.Name)
		if st := teammateState[m.State]; st != "" {
			line += " · " + st
		}
		online = append(online, line)
	}
	for i, m := range team.Top {
		top = append(top, fmt.Sprintf("%d. %s — %d", i+1, who(m.Name), m.Balance))
	}
	return online, top
}

func (t *tray) setIcon(idle bool) {
	if t.idle != nil && *t.idle == idle {
		return
	}
	t.idle = &idle
	icon := trayIcon(idle)
	if runtime.GOOS == "darwin" {
		systray.SetTemplateIcon(icon, icon)
	} else {
		systray.SetIcon(icon)
	}
}

func (t *tray) loop() {
	for i, it := range t.pauseItems {
		h := PauseHours[i]
		go func(it *systray.MenuItem) {
			for range it.ClickedCh {
				t.setPause(time.Duration(h) * time.Hour)
			}
		}(it)
	}
	for {
		select {
		case <-t.ctx.Done():
			return
		case <-t.request.ClickedCh:
			go t.withDialog(t.doRequest)
		case <-t.requests.ClickedCh:
			go t.withDialog(t.showRequests)
		case <-t.stop.ClickedCh:
			if t.agent.StopCurrent() {
				trayNotifier{}.Notify(appTitle, "Ревью остановлено, задание уйдёт другому")
			}
		case <-t.lastLog.ClickedCh:
			if p := t.agent.State().LastLog; p != "" {
				openFile(p)
			}
		case <-t.unpause.ClickedCh:
			t.setPause(0)
		case <-t.update.ClickedCh:
			openURL(client.ReleasesURL)
		case <-t.settings.ClickedCh:
			if _, err := t.store.EnsureSettings(); err != nil {
				zenity.Error(err.Error(), zenity.Title(appTitle))
			} else {
				openEditor(t.store.Path())
			}
		case <-t.autostart.ClickedCh:
			var err error
			if t.autostart.Checked() {
				err = uninstall()
			} else {
				err = install()
			}
			if err != nil {
				zenity.Error(err.Error(), zenity.Title(appTitle))
			}
			if autostartInstalled() {
				t.autostart.Check()
			} else {
				t.autostart.Uncheck()
			}
		case <-t.quit.ClickedCh:
			t.agent.StopCurrent()
			systray.Quit()
			return
		}
	}
}

func (t *tray) setPause(d time.Duration) {
	if err := t.agent.SetPause(d); err != nil {
		zenity.Error(err.Error(), zenity.Title(appTitle))
	}
}

// withDialog не даёт открыть два окна сразу: второй клик, пока окно открыто, игнорируется.
func (t *tray) withDialog(fn func() error) {
	if !t.dialog.TryLock() {
		return
	}
	defer t.dialog.Unlock()
	if err := fn(); err != nil && !errors.Is(err, zenity.ErrCanceled) {
		zenity.Error(err.Error(), zenity.Title(appTitle))
	}
	t.refresh()
}

func (t *tray) refresh() {
	if t.status == nil {
		return
	}
	t.refreshMu.Lock()
	defer t.refreshMu.Unlock()
	st := t.agent.State()
	c := t.store.Get()
	switch {
	case st.SettingsErr != "" && !st.Configured:
		t.status.SetTitle("Настройки: " + shorten(st.SettingsErr, 60))
	case !st.Configured && st.LastError != "":
		t.status.SetTitle(shorten(st.LastError, 70))
	case !st.Configured:
		t.status.SetTitle("Подключаюсь к брокеру…")
	case st.LastError != "":
		t.status.SetTitle("Нет связи: " + shorten(st.LastError, 60))
	case !st.PausedUntil.IsZero():
		t.status.SetTitle(fmt.Sprintf("%s · пауза до %s · баланс %d", st.Name, st.PausedUntil.Format("15:04"), st.Balance))
	case st.Online:
		t.status.SetTitle(fmt.Sprintf("%s · онлайн · баланс %d", st.Name, st.Balance))
	default:
		t.status.SetTitle(st.Name + " · подключаюсь…")
	}
	t.setIcon(!st.Configured || !st.Online || !st.PausedUntil.IsZero())
	systray.SetTooltip(appTitle + ": " + t.status.String())
	if st.Update != "" {
		t.update.SetTitle("Вышла " + st.Update + " — скачать")
		t.update.Show()
	} else {
		t.update.Hide()
	}
	if st.SettingsErr != "" {
		t.settings.SetTitle("Настройки… — в файле ошибка")
	} else {
		t.settings.SetTitle("Настройки…")
	}
	// Об ошибке в файле — уведомлением один раз: пока её не исправят, действуют прежние настройки.
	if st.SettingsErr != t.shownErr {
		t.shownErr = st.SettingsErr
		if st.SettingsErr != "" {
			trayNotifier{}.Notify(appTitle, "Ошибка в настройках: "+st.SettingsErr)
		}
	}
	t.request.SetTitle(fmt.Sprintf("Попросить ревью MR из буфера · баланс %d", st.Balance))
	if cur := st.Current; cur != nil {
		t.current.SetTitle(fmt.Sprintf("Ревьюю %s · просит %s · %d мин", cur.Short, cur.Requester, int(time.Since(cur.Started).Minutes())))
		t.stop.Show()
	} else {
		t.current.SetTitle("Заданий нет")
		t.stop.Hide()
	}
	if st.LastLog != "" {
		t.lastLog.Show()
	} else {
		t.lastLog.Hide()
	}
	if st.PausedUntil.IsZero() {
		t.pause.SetTitle("Пауза")
		t.unpause.Hide()
	} else {
		t.pause.SetTitle("Пауза до " + st.PausedUntil.Format("02.01 15:04"))
		t.unpause.Show()
	}
	ignored := map[string]bool{}
	for _, r := range st.Ignored {
		ignored[r] = true
	}
	repos := make([]string, 0, len(c.Projects))
	for _, p := range c.Projects {
		if ignored[p.Repo] {
			repos = append(repos, p.Repo+" — брокер не разрешает")
		} else {
			repos = append(repos, p.Repo)
		}
	}
	fillSlots(t.projSlots, repos)
	// Команду показываем, только пока есть связь: без неё список устарел бы молча.
	online, top := teamLines(st.Team, st.Name)
	if st.Online && len(top) > 0 {
		t.online.SetTitle(fmt.Sprintf("Онлайн: %d", len(online)))
		fillSlots(t.onlineSlots, online)
		fillSlots(t.topSlots, top)
		t.online.Show()
		t.top.Show()
	} else {
		t.online.Hide()
		t.top.Hide()
	}
}

// ask — пришло задание: окно «Принять / Отклонить» со ссылкой на MR. Системное уведомление с
// кнопками на macOS доступно только подписанному приложению, поэтому вопрос — окно поверх
// остальных, а уведомление со звуком зовёт к нему. Окно закрывается само, когда ctx истекает.
func (t *tray) ask(ctx context.Context, o client.JobOffer) bool {
	trayNotifier{}.Notify(appTitle, fmt.Sprintf("%s просит ревью %s", o.Requester, o.MR.Short()))
	text := fmt.Sprintf("%s просит отревьюить %s\n%s\n\nПринять — запущу %s в %s.\nНе ответишь до %s — задание уйдёт другому.",
		o.Requester, o.MR.Short(), o.MR, o.Command, o.Workdir, o.Until.Format("15:04"))
	for {
		err := zenity.Question(text, zenity.Title("Запрос на ревью"), zenity.OKLabel("Принять"),
			zenity.CancelLabel("Отклонить"), zenity.ExtraButton("Открыть MR"), zenity.Context(ctx))
		switch {
		case err == nil:
			return true
		case errors.Is(err, zenity.ErrExtraButton):
			openURL(o.MR.String()) // и спросить снова
		case errors.Is(err, zenity.ErrCanceled) || ctx.Err() != nil:
			return false
		default:
			trayNotifier{}.Notify(appTitle, "Окно с вопросом не открылось, задание ушло другому: "+err.Error())
			return false
		}
	}
}

func shorten(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func (t *tray) doRequest() error {
	text, _ := clipboard.ReadAll()
	if _, err := mrurl.NormalizeMR(text); err != nil {
		s, err := zenity.Entry("В буфере нет ссылки на MR. Вставь её:", zenity.Title("Попросить ревью"))
		if err != nil {
			return err
		}
		text = s
	}
	ctx, cancel := context.WithTimeout(t.ctx, 30*time.Second)
	defer cancel()
	mr, r, err := t.agent.RequestReview(ctx, text)
	if err != nil {
		return err
	}
	trayNotifier{}.Notify(appTitle, client.DescribeRequest(mr, r))
	return nil
}

func (t *tray) showRequests() error {
	c := t.store.Get()
	ctx, cancel := context.WithTimeout(t.ctx, 30*time.Second)
	defer cancel()
	list, err := client.NewAPI(c.Broker, c.Key).MyRequests(ctx)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		return zenity.Info("Запросов не было.", zenity.Title("Мои запросы"))
	}
	var b strings.Builder
	for _, r := range list {
		short := r.MRURL
		if mr, err := mrurl.ParseMR(r.MRURL); err == nil {
			short = mr.Short()
		}
		var jobs []string
		for _, j := range r.Jobs {
			who := j.Reviewer
			if who == "" {
				who = "ждёт ревьюера"
			}
			jobs = append(jobs, who+": "+jobStatus(j.Status))
		}
		fmt.Fprintf(&b, "%s  %s — %s\n    %s\n", time.Unix(r.CreatedAt, 0).Format("02.01 15:04"), short, requestStatus(r.Status), strings.Join(jobs, "; "))
	}
	return zenity.Info(b.String(), zenity.Title("Мои запросы"))
}

// openEditor открывает файл настроек в текстовом редакторе.
func openEditor(path string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", "-t", path)
	case "windows":
		cmd = exec.Command("notepad.exe", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	startDetached(cmd)
}

// openURL открывает ссылку в браузере.
func openURL(u string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	startDetached(cmd)
}

// openFile открывает лог программой по умолчанию.
func openFile(path string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "windows":
		cmd = exec.Command("notepad.exe", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	startDetached(cmd)
}

// startDetached запускает программу и забирает её код выхода, когда она закончит: без Wait на
// unix от каждого клика оставался бы процесс-зомби.
func startDetached(cmd *exec.Cmd) {
	if cmd.Start() == nil {
		go cmd.Wait()
	}
}
