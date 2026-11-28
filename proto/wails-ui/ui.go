package main

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// UI — служба, которую видит JS. Каждый её экспортированный метод вызывается из страницы, поэтому
// здесь только намерения: «прочитать состояние», «нажата кнопка тоста N», «попросить ревью по
// ссылке». Ни команды, ни пути, ни адреса из JS не принимаются: ссылку на MR, которую открывает
// кнопка тоста, Go знает сам.
type UI struct {
	app     *application.App
	popover *application.WebviewWindow
	toasts  *toaster

	mu       sync.Mutex
	paused   time.Time
	injected int            // сколько HTML-элементов страница нашла внутри строк брокера: должно быть 0
	calls    map[string]int // какие методы звала страница: для отчёта, мост JS → Go жив
}

func (u *UI) count(name string) {
	u.mu.Lock()
	u.calls[name]++
	u.mu.Unlock()
}

func (u *UI) callCounts() map[string]int {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := map[string]int{}
	for k, v := range u.calls {
		out[k] = v
	}
	return out
}

func newUI() *UI { return &UI{calls: map[string]int{}} }

// Member — участник в списках «Онлайн» и «Топ».
type Member struct {
	Name    string
	State   string
	Balance int
	Me      bool
}

// Job — ревью одного ревьюера по моему запросу.
type Job struct {
	Reviewer string
	Status   string
}

// Request — мой запрос на ревью.
type Request struct {
	When   string
	Short  string
	Status string
	Jobs   []Job
}

// Current — ревью, которое идёт сейчас.
type Current struct {
	Short     string
	Requester string
	Minutes   int
}

// State — всё, что показывает окно у трея.
type State struct {
	Name        string
	Online      bool
	Balance     int
	PausedUntil string
	Current     *Current
	Team        []Member
	Top         []Member
	Requests    []Request
	Projects    []string
	SettingsErr string
}

// hostile — имя участника, как его мог бы прислать враждебный брокер. На странице оно должно
// показаться текстом, а не стать картинкой с обработчиком.
const hostile = `<img src=x onerror="document.title='pwned'">Вася`

// State отдаёт состояние для окна.
func (u *UI) State() State {
	u.count("State")
	u.mu.Lock()
	defer u.mu.Unlock()
	st := State{
		Name:    "georgiy",
		Online:  true,
		Balance: 7,
		Current: &Current{Short: "backend!412", Requester: "anna", Minutes: 4},
		Team: []Member{
			{Name: "georgiy", State: "ревьюит", Me: true},
			{Name: "anna", State: "свободна"},
			{Name: hostile, State: "пауза"},
		},
		Top: []Member{
			{Name: "anna", Balance: 12},
			{Name: "georgiy", Balance: 7, Me: true},
			{Name: hostile, Balance: 3},
		},
		Requests: []Request{
			{When: "28.11 14:05", Short: "web!1031", Status: "идёт", Jobs: []Job{{Reviewer: "anna", Status: "ревьюит"}, {Reviewer: "", Status: "ждёт"}}},
			{When: "27.11 18:40", Short: "backend!398", Status: "закрыт", Jobs: []Job{{Reviewer: hostile, Status: "готово"}}},
		},
		Projects:    []string{"gitlab.example.com/team/backend", "gitlab.example.com/team/web — брокер не разрешает"},
		SettingsErr: "",
	}
	if !u.paused.IsZero() && time.Now().Before(u.paused) {
		st.PausedUntil = u.paused.Format("02.01 15:04")
		st.Current = nil
	}
	return st
}

// Pause ставит паузу на hours часов; 0 снимает её. Часы — только из списка, как в CLI.
func (u *UI) Pause(hours int) error {
	switch hours {
	case 0, 1, 3, 6, 12, 24:
	default:
		return fmt.Errorf("пауза: 1, 3, 6, 12 или 24 часа")
	}
	u.mu.Lock()
	if hours == 0 {
		u.paused = time.Time{}
	} else {
		u.paused = time.Now().Add(time.Duration(hours) * time.Hour)
	}
	u.mu.Unlock()
	u.app.Event.Emit("state")
	return nil
}

// Request просит ревью MR. В прототипе — только проверка вида ссылки; в клиенте здесь будет
// Agent.RequestReview со строгим разбором mrurl.
func (u *UI) Request(link string) (string, error) {
	link = strings.TrimSpace(link)
	if !strings.HasPrefix(link, "https://") || !strings.Contains(link, "/-/merge_requests/") {
		return "", fmt.Errorf("это не ссылка на MR")
	}
	return "Запрос ушёл anna — уведомлю, кто возьмёт. Баланс 6", nil
}

// OpenSettings открыл бы файл настроек в редакторе. Путь Go знает сам.
func (u *UI) OpenSettings() {
	u.toasts.show(toastSpec{Title: "cross-review", Body: "Здесь откроется ~/cross-review.yaml в редакторе", Seconds: 5})
}

// StopCurrent остановил бы текущее ревью.
func (u *UI) StopCurrent() {
	u.toasts.show(toastSpec{Title: "cross-review", Body: "Ревью остановлено, задание уйдёт другому", Seconds: 5})
}

// HidePopover прячет окно у трея.
func (u *UI) HidePopover() { u.popover.Hide() }

// Toast отдаёт странице тоста, что в нём показать.
func (u *UI) Toast(id int64) (toastSpec, error) {
	u.count("Toast")
	return u.toasts.spec(id)
}

// ToastAction — нажата кнопка action тоста id; -1 — клик по тосту без кнопок.
func (u *UI) ToastAction(id int64, action int) { u.toasts.action(id, action) }

// ToastResize — страница тоста измерила свою высоту.
func (u *UI) ToastResize(id int64, height int) {
	u.count("ToastResize")
	u.toasts.resize(id, height)
}

// RenderCheck — страница сообщает, сколько элементов <img> оказалось в DOM после отрисовки строк
// брокера. Больше нуля — строку вставили как HTML.
func (u *UI) RenderCheck(images int) {
	u.count("RenderCheck")
	u.mu.Lock()
	u.injected += images
	u.mu.Unlock()
}

func (u *UI) demoNotify() {
	u.toasts.show(toastSpec{Title: "cross-review", Body: "web!1031: anna взяла ревью", Seconds: 8})
}

// demoOffer — вопрос о задании, как его задаст агент. Ответ ждём, пока не истечёт отсчёт.
func (u *UI) demoOffer() {
	const mr = "https://gitlab.example.com/team/backend/-/merge_requests/412"
	until := time.Now().Add(60 * time.Second)
	id, answers := u.toasts.show(toastSpec{
		Title:   "Запрос на ревью",
		Body:    hostile + " просит отревьюить backend!412",
		Footer:  "Принять — запущу just review <LINK>\nв ~/work/backend\nНе ответишь до " + until.Format("15:04:05") + " — задание уйдёт другому",
		Buttons: []string{"Принять", "Отклонить", "Открыть MR"},
		Primary: 0,
		Seconds: 60,
	})
	timeout := time.After(time.Until(until))
	for {
		select {
		case a := <-answers:
			switch a {
			case 2:
				openURL(mr) // тост остаётся: человек посмотрит MR и ответит
				continue
			case 0:
				u.toasts.close(id)
				u.toasts.show(toastSpec{Title: "cross-review", Body: "Ревьюю backend!412, просит " + hostile, Seconds: 6})
			default:
				u.toasts.close(id)
			}
			return
		case <-timeout:
			u.toasts.close(id)
			u.toasts.show(toastSpec{Title: "cross-review", Body: "backend!412: ответа не было, задание ушло другому", Seconds: 6})
			return
		}
	}
}

func (u *UI) injectedCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.injected
}
