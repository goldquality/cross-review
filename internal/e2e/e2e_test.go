// Сквозной тест: настоящий брокер по HTTP и три клиента с разными папками, как у участников.
package e2e

import (
	"context"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goldquality/cross-review/internal/broker"
	"github.com/goldquality/cross-review/internal/client"
)

const (
	repoURL = "https://gitlab.example.com/acme/shop"
	mrURL   = "https://gitlab.example.com/acme/shop/-/merge_requests/9"
)

type notes struct {
	mu   sync.Mutex
	list []string
}

func (n *notes) Notify(_, text string) {
	n.mu.Lock()
	n.list = append(n.list, text)
	n.mu.Unlock()
}

func (n *notes) count(sub string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	c := 0
	for _, s := range n.list {
		if strings.Contains(s, sub) {
			c++
		}
	}
	return c
}

func (n *notes) has(sub string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, s := range n.list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

type member struct {
	name  string
	home  string
	store *client.Store
	agent *client.Agent
	notes *notes
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("не дождался:", what)
}

func TestReviewRound(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("команда ревью — sh-скрипт")
	}
	db, err := broker.OpenDB(filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := broker.NewService(db)
	svc.Version = "v1.0.0"
	invite, _, _ := svc.EnsureInvite()
	srv := httptest.NewServer(svc.Handler(log.New(io.Discard, "", 0), false))
	defer srv.Close()

	root := t.TempDir()
	script := filepath.Join(root, "review.sh")
	os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$#\" \"$@\" > \"$CROSS_REVIEW_HOME/args\"\n"), 0o755)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var members []*member
	for _, name := range []string{"Вася", "Петя", "Маша"} {
		// Каждый участник — только файл настроек, как его присылает администратор; ключ клиент
		// получает сам по приглашению.
		home := filepath.Join(root, name)
		os.MkdirAll(home, 0o755)
		settings := filepath.Join(home, client.SettingsFile)
		os.WriteFile(settings, []byte("broker: "+srv.URL+"\ninvite: "+invite+"\nname: "+name+
			"\ncommand: "+script+" review <LINK>\nworkdir: "+root+"\nprojects:\n  - repo: "+repoURL+"\n"), 0o644)
		store, err := client.OpenStore(home, settings)
		if err != nil {
			t.Fatal(err)
		}
		if c := store.Get(); c.Err != "" {
			t.Fatal(c.Err)
		}
		n := &notes{}
		version := "e2e"
		if name == "Вася" {
			version = "v0.9.0"
		}
		a := &client.Agent{Store: store, Notify: n, LogDir: filepath.Join(home, "logs"), Version: version}
		m := &member{name: name, home: home, store: store, agent: a, notes: n}
		members = append(members, m)
	}
	// Скрипт пишет аргументы в $CROSS_REVIEW_HOME/args: команда ревью получает окружение клиента.
	t.Setenv("CROSS_REVIEW_HOME", root)
	for _, m := range members {
		go m.agent.Run(ctx)
	}
	vasya := members[0]
	waitFor(t, "все онлайн", func() bool {
		for _, m := range members {
			if !m.agent.State().Online {
				return false
			}
		}
		return true
	})

	// Вася на старой версии: брокер вышел из нового тега, трей подсказывает обновиться — один раз.
	vasya.agent.Kick()
	waitFor(t, "напоминание об обновлении", func() bool {
		return vasya.agent.State().Update == "v1.0.0" && vasya.notes.has("Вышла cross-review v1.0.0, у тебя v0.9.0")
	})
	if u := members[1].agent.State().Update; u != "" || members[1].notes.has("Вышла") {
		t.Fatal("сборке без версии напоминание не нужно:", u)
	}

	mr, r, err := vasya.agent.RequestReview(ctx, mrURL+"/diffs#note_1")
	if err != nil {
		t.Fatal(err)
	}
	if mr.String() != mrURL || len(r.Reviewers) != 1 || r.Balance != 1 {
		t.Fatal(mr, r)
	}
	reviewer := members[1]
	if r.Reviewers[0] == "Маша" {
		reviewer = members[2]
	}
	waitFor(t, "ревью выполнено", func() bool { return reviewer.notes.has("ревью готово, +1") })
	args, err := os.ReadFile(filepath.Join(root, "args"))
	if err != nil {
		t.Fatal(err)
	}
	if string(args) != "2\nreview\n"+mrURL+"\n" {
		t.Fatalf("команда получила %q", args)
	}
	reviewer.agent.Kick()
	waitFor(t, "баланс ревьюера 3", func() bool { return reviewer.agent.State().Balance == 3 })
	vasya.agent.Kick()
	waitFor(t, "уведомление просящему", func() bool { return vasya.notes.has("закончил ревью") })
	if vasya.agent.State().Balance != 1 {
		t.Fatal(vasya.agent.State())
	}

	// Пауза: задания перестают приходить, просить можно.
	for _, m := range members[1:] {
		if err := m.agent.SetPause(time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(300 * time.Millisecond)
	_, r2, err := vasya.agent.RequestReview(ctx, "https://gitlab.example.com/acme/shop/-/merge_requests/10")
	if err != nil {
		t.Fatal(err)
	}
	if !r2.Waiting || r2.Balance != 0 {
		t.Fatal("на паузе никто не должен получить задание:", r2)
	}
	if _, _, err := vasya.agent.RequestReview(ctx, "https://gitlab.example.com/acme/shop/-/merge_requests/11"); err == nil || !strings.Contains(err.Error(), "баллов нет") {
		t.Fatal("баланс 0:", err)
	}
	if _, _, err := vasya.agent.RequestReview(ctx, "https://gitlab.example.com/acme/other/-/merge_requests/1"); err == nil || !strings.Contains(err.Error(), "нет в настройках") {
		t.Fatal("чужой проект:", err)
	}
	// За весь круг Вася отметился много раз, а о новой версии ему сказали один раз.
	if c := vasya.notes.count("Вышла cross-review"); c != 1 {
		t.Fatal("напоминаний об обновлении:", c)
	}
}
