package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
)

type trayNotifier struct{}

func (trayNotifier) Notify(title, text string) {
	zenity.Notify(text, zenity.Title(title))
}

type tray struct {
	dir    string
	store  *client.Store
	agent  *client.Agent
	ctx    context.Context
	dialog sync.Mutex // одно окно за раз

	status, current, stop, lastLog *systray.MenuItem
	request, requests              *systray.MenuItem
	projects, addProject           *systray.MenuItem
	projSlots                      []*systray.MenuItem
	pause, unpause                 *systray.MenuItem
	pauseItems                     []*systray.MenuItem
	connect, autostart, quit       *systray.MenuItem
	idle                           *bool
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
	systray.Run(t.onReady, cancel)
	return nil
}

func (t *tray) onReady() {
	t.setIcon(true)
	systray.SetTooltip(appTitle)

	t.status = systray.AddMenuItem("…", "")
	t.status.Disable()
	systray.AddSeparator()
	t.request = systray.AddMenuItem("Попросить ревью MR из буфера", "Ссылка на MR из буфера обмена; запрос стоит 1 балл")
	t.requests = systray.AddMenuItem("Мои запросы", "")
	systray.AddSeparator()
	t.current = systray.AddMenuItem("Заданий нет", "")
	t.current.Disable()
	t.stop = systray.AddMenuItem("Остановить ревью", "Задание уйдёт другому участнику")
	t.lastLog = systray.AddMenuItem("Открыть лог последнего ревью", "")
	systray.AddSeparator()
	t.projects = systray.AddMenuItem("Проекты", "")
	t.addProject = t.projects.AddSubMenuItem("Добавить проект…", "")
	for i := 0; i < maxProjSlots; i++ {
		s := t.projects.AddSubMenuItem("", "")
		s.Hide()
		t.projSlots = append(t.projSlots, s)
	}
	t.pause = systray.AddMenuItem("Пауза", "На паузе задания не приходят; просить ревью можно")
	for _, h := range PauseHours {
		t.pauseItems = append(t.pauseItems, t.pause.AddSubMenuItem(fmt.Sprintf("%d ч", h), ""))
	}
	t.unpause = t.pause.AddSubMenuItem("Снять паузу", "")
	systray.AddSeparator()
	t.connect = systray.AddMenuItem("Подключиться к брокеру…", "")
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
	if !t.agent.State().Configured {
		go t.withDialog(t.doConnect)
	}
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
	for i, it := range t.projSlots {
		go func(i int, it *systray.MenuItem) {
			for range it.ClickedCh {
				go t.withDialog(func() error { return t.editProject(i) })
			}
		}(i, it)
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
		case <-t.addProject.ClickedCh:
			go t.withDialog(t.doAddProject)
		case <-t.unpause.ClickedCh:
			t.setPause(0)
		case <-t.connect.ClickedCh:
			go t.withDialog(t.doConnect)
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
	st := t.agent.State()
	c := t.store.Get()
	switch {
	case !st.Configured:
		t.status.SetTitle("Не подключён к брокеру")
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
	if st.Configured {
		t.connect.SetTitle("Сменить брокер…")
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
	for i, s := range t.projSlots {
		if i < len(c.Projects) {
			s.SetTitle(c.Projects[i].Repo)
			s.Show()
		} else {
			s.Hide()
		}
	}
	if len(c.Projects) >= maxProjSlots {
		t.addProject.Disable()
	} else {
		t.addProject.Enable()
	}
}

func shorten(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func (t *tray) doConnect() error {
	opts := []zenity.Option{zenity.Title(appTitle)}
	c := t.store.Get()
	brokerURL, err := zenity.Entry("Адрес брокера, например https://review.example.com", append(opts, zenity.EntryText(c.Broker))...)
	if err != nil {
		return err
	}
	invite, err := zenity.Entry("Приглашение от администратора брокера (inv_…)", opts...)
	if err != nil {
		return err
	}
	name, err := zenity.Entry("Как тебя показывать команде", append(opts, zenity.EntryText(c.Name))...)
	if err != nil {
		return err
	}
	gitlabUser, err := zenity.Entry("Вход в GitLab (необязательно)", opts...)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(t.ctx, 30*time.Second)
	defer cancel()
	if err := doJoin(ctx, t.store, brokerURL, invite, name, gitlabUser, func(s string) { trayNotifier{}.Notify(appTitle, s) }); err != nil {
		return err
	}
	t.agent.Kick()
	if len(t.store.Get().Projects) == 0 {
		return t.doAddProject()
	}
	return nil
}

func (t *tray) doAddProject() error {
	opts := []zenity.Option{zenity.Title("Новый проект")}
	repoURL, err := zenity.Entry("Адрес репо в GitLab, например https://gitlab.example.com/acme/api", opts...)
	if err != nil {
		return err
	}
	repo, err := mrurl.ParseRemote(repoURL)
	if err != nil {
		return err
	}
	clones, err := t.scan(repo)
	if err != nil {
		return err
	}
	const manual = "Указать папку вручную…"
	clone := ""
	if len(clones) > 0 {
		pick, err := zenity.List("Клоны "+repo.String()+" на диске:", append(clones, manual), append(opts, zenity.DefaultItems(clones[0]))...)
		if err != nil {
			return err
		}
		if pick != manual {
			clone = pick
		}
	} else {
		zenity.Info("Клонов "+repo.String()+" в домашней папке не нашлось. Укажи папку клона.", opts...)
	}
	if clone == "" {
		if clone, err = zenity.SelectFile(append(opts, zenity.Directory())...); err != nil {
			return err
		}
	}
	workdir := clone
	err = zenity.Question("Где запускать команду ревью?\n\n"+clone, append(opts, zenity.OKLabel("В папке клона"), zenity.ExtraButton("В другой папке…"), zenity.CancelLabel("Отмена"))...)
	switch {
	case errors.Is(err, zenity.ErrExtraButton):
		if workdir, err = zenity.SelectFile(append(opts, zenity.Directory(), zenity.Filename(filepath.Dir(clone)+string(filepath.Separator)))...); err != nil {
			return err
		}
	case err != nil:
		return err
	}
	command, err := zenity.Entry("Команда ревью. Вместо <LINK> подставится ссылка на MR; запуск без шелла, в папке\n"+workdir,
		append(opts, zenity.EntryText("./dev review <LINK>"))...)
	if err != nil {
		return err
	}
	p, err := client.NewProject(repoURL, clone, workdir, command)
	if err != nil {
		return err
	}
	if err := addProject(t.store, p); err != nil {
		return err
	}
	t.agent.Kick()
	trayNotifier{}.Notify(appTitle, "Проект "+p.Repo+" подключён")
	return nil
}

func (t *tray) scan(repo mrurl.Repo) ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(t.ctx, 2*time.Minute)
	defer cancel()
	dlg, _ := zenity.Progress(zenity.Title("Поиск клонов"), zenity.Pulsate(), zenity.NoCancel())
	if dlg != nil {
		dlg.Text("Ищу клоны " + repo.String() + " в домашней папке…")
	}
	var found []string
	err = client.Scan(ctx, home, client.ScanDepth, repo, func(p string) { found = append(found, p) })
	if dlg != nil {
		dlg.Close()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		err = nil // что нашли за две минуты, то и показываем
	}
	return found, err
}

func (t *tray) editProject(i int) error {
	c := t.store.Get()
	if i >= len(c.Projects) {
		return nil
	}
	p := c.Projects[i]
	opts := []zenity.Option{zenity.Title(p.Repo)}
	const (
		cmd    = "Изменить команду"
		wd     = "Изменить папку запуска"
		run    = "Проверить: прогнать на MR…"
		remove = "Отключить проект"
	)
	info := fmt.Sprintf("Клон: %s\nПапка запуска: %s\nКоманда: %s", p.Clone, p.Workdir, client.FormatCommand(p.Command))
	pick, err := zenity.List(info, []string{cmd, wd, run, remove}, opts...)
	if err != nil {
		return err
	}
	switch pick {
	case cmd:
		s, err := zenity.Entry("Команда ревью, вместо <LINK> — ссылка на MR", append(opts, zenity.EntryText(client.FormatCommand(p.Command)))...)
		if err != nil {
			return err
		}
		argv, err := client.ParseCommand(s)
		if err != nil {
			return err
		}
		p.Command = argv
	case wd:
		s, err := zenity.SelectFile(append(opts, zenity.Directory(), zenity.Filename(p.Workdir+string(filepath.Separator)))...)
		if err != nil {
			return err
		}
		p.Workdir = s
	case run:
		link, err := zenity.Entry("Ссылка на MR этого проекта: команда запустится как при задании, лог откроется", opts...)
		if err != nil {
			return err
		}
		mr, err := mrurl.NormalizeMR(link)
		if err != nil {
			return err
		}
		res, err := client.Run(t.ctx, p, mr, filepath.Join(t.dir, "logs"), client.ChildEnv(t.agent.ShellPath))
		if err != nil {
			return err
		}
		openFile(res.LogPath)
		return nil
	case remove:
		if err := zenity.Question("Отключить "+p.Repo+"? Задания по нему перестанут приходить.", opts...); err != nil {
			return err
		}
		if err := removeProject(t.store, p.Repo); err != nil {
			return err
		}
		t.agent.Kick()
		return nil
	default:
		return nil
	}
	if err := addProject(t.store, p); err != nil {
		return err
	}
	t.agent.Kick()
	return nil
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
	cmd.Start()
}
