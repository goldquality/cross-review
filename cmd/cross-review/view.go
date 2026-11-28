package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/goldquality/cross-review/internal/client"
	"github.com/goldquality/cross-review/internal/mrurl"
)

// Модель окна у трея и тостов: чистые функции от состояния агента, без Wails, чтобы их проверяли
// тесты. Всё, что пришло от брокера (имена, тексты ошибок, ссылки из «Моих запросов»), проходит
// client.Clean здесь же; страница кладёт строки только в textContent.

// View — что показывает окно у трея.
type View struct {
	Version     string
	Name        string
	Status      string // строка под именем
	Kind        string // online, offline, paused, reviewing — цвет точки у имени
	Balance     int
	Configured  bool
	Online      bool
	PausedUntil string // «02.01 15:04», пусто — паузы нет
	Update      string // вышедшая версия, пусто — обновляться не нужно
	Current     *CurrentView
	Team        []MemberView // кто онлайн; пусто, пока нет связи
	Top         []MemberView
	Projects    []ProjectView
	Settings    string // файл настроек
	SettingsErr string
	LastLog     bool // есть лог последнего ревью
	Autostart   bool
	PauseHours  []int
}

// CurrentView — ревью, которое идёт сейчас.
type CurrentView struct {
	Short     string
	Requester string
	Started   int64 // unix, мс: минуты считает страница
}

// MemberView — участник в списках «Онлайн» и «Топ по баллам».
type MemberView struct {
	Name    string
	State   string
	Balance int
	Me      bool
}

// ProjectView — проект из настроек.
type ProjectView struct {
	Repo    string
	Ignored bool // брокер не разрешает этот репо
}

// RequestView — мой запрос на ревью.
type RequestView struct {
	When   string
	Short  string
	Status string
	Open   bool
	Jobs   string // кто и в каком состоянии, одной строкой
}

var stateKind = map[trayState]string{stateOnline: "online", stateOffline: "offline", statePaused: "paused", stateReviewing: "reviewing"}

var teammateState = map[string]string{"free": "свободен", "busy": "ревьюит", "paused": "пауза"}

// buildView собирает модель окна из состояния агента и настроек.
func buildView(st client.State, projects []client.Project, autostart bool) View {
	v := View{
		Version:    version,
		Name:       st.Name,
		Kind:       stateKind[iconState(st)],
		Status:     statusLine(st),
		Balance:    st.Balance,
		Configured: st.Configured,
		Online:     st.Online,
		Update:     client.Clean(st.Update),
		Settings:   st.Settings,
		// Ошибка разбора YAML — не от брокера, но в ней бывают куски файла: чистим и её.
		SettingsErr: client.Clean(st.SettingsErr),
		LastLog:     st.LastLog != "",
		Autostart:   autostart,
		PauseHours:  PauseHours,
	}
	if v.Name == "" {
		v.Name = "cross-review"
	}
	if !st.PausedUntil.IsZero() {
		v.PausedUntil = st.PausedUntil.Format("02.01 15:04")
	}
	if cur := st.Current; cur != nil {
		v.Current = &CurrentView{Short: cur.Short, Requester: client.Clean(cur.Requester), Started: cur.Started.UnixMilli()}
	}
	// Команду показываем, только пока есть связь: без неё список устарел бы молча.
	if st.Online && len(st.Team.Top) > 0 {
		v.Team, v.Top = teamViews(st.Team, st.Name)
	}
	ignored := map[string]bool{}
	for _, r := range st.Ignored {
		ignored[r] = true
	}
	for _, p := range projects {
		v.Projects = append(v.Projects, ProjectView{Repo: p.Repo, Ignored: ignored[p.Repo]})
	}
	return v
}

// statusLine — строка состояния под именем и подсказка иконки трея.
func statusLine(st client.State) string {
	switch {
	case st.SettingsErr != "" && !st.Configured:
		return "Ошибка в настройках: " + client.Clean(st.SettingsErr)
	case !st.Configured && st.LastError != "":
		return client.Clean(st.LastError)
	case !st.Configured:
		return "Подключаюсь к брокеру…"
	case st.LastError != "":
		return "Нет связи: " + client.Clean(st.LastError)
	case st.Current != nil:
		return "ревьюю " + st.Current.Short
	case !st.PausedUntil.IsZero():
		return "пауза до " + st.PausedUntil.Format("15:04") + " — задания не приходят"
	case st.Online:
		return "онлайн, жду заданий"
	}
	return "подключаюсь…"
}

// teamViews — списки «Онлайн» и «Топ по баллам»; себя участник видит с пометкой.
func teamViews(team client.Team, me string) (online, top []MemberView) {
	for _, m := range team.Online {
		online = append(online, MemberView{Name: client.Clean(m.Name), State: teammateState[m.State], Me: strings.EqualFold(m.Name, me)})
	}
	for _, m := range team.Top {
		top = append(top, MemberView{Name: client.Clean(m.Name), Balance: m.Balance, Me: strings.EqualFold(m.Name, me)})
	}
	return online, top
}

// requestViews — «Мои запросы» по ответу брокера.
func requestViews(list []client.RequestInfo) []RequestView {
	out := make([]RequestView, 0, len(list))
	for _, r := range list {
		short := client.Clean(r.MRURL)
		if mr, err := mrurl.ParseMR(r.MRURL); err == nil {
			short = mr.Short()
		}
		var jobs []string
		for _, j := range r.Jobs {
			who := client.Clean(j.Reviewer)
			if who == "" {
				who = "ждёт ревьюера"
			}
			jobs = append(jobs, who+": "+jobStatus(j.Status))
		}
		out = append(out, RequestView{
			When:   time.Unix(r.CreatedAt, 0).Format("02.01 15:04"),
			Short:  short,
			Status: requestStatus(r.Status),
			Open:   r.Status == "open",
			Jobs:   strings.Join(jobs, "; "),
		})
	}
	return out
}

// offerLines — тексты вопроса о задании.
type offerLines struct {
	title string
	who   string // кто и что просит
	run   string // какая команда запустится и где: папка с новой строки, чтобы путь не рвался
	until string // до какого времени ждём ответа
}

// offerText — тексты вопроса о задании. Имя просящего пришло от брокера, поэтому проходит
// client.Clean; команда и папка — из настроек участника.
func offerText(o client.JobOffer) offerLines {
	dir := o.Workdir
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(dir, home) {
		dir = "~" + strings.TrimPrefix(dir, home)
	}
	return offerLines{
		title: "Запрос на ревью",
		who:   fmt.Sprintf("%s просит отревьюить %s", client.Clean(o.Requester), o.MR.Short()),
		run:   fmt.Sprintf("Принять — запущу %s\nв %s", shorten(o.Command, 80), dir),
		until: "Не ответишь до " + o.Until.Format("15:04") + " — задание уйдёт другому",
	}
}

func shorten(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
