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
	invite, _, _ := svc.EnsureInvite()
	srv := httptest.NewServer(svc.Handler(log.New(io.Discard, "", 0), false))
	defer srv.Close()

	root := t.TempDir()
	clone := filepath.Join(root, "shop")
	os.MkdirAll(filepath.Join(clone, ".git"), 0o755)
	os.WriteFile(filepath.Join(clone, ".git", "config"), []byte("[remote \"origin\"]\n\turl = git@gitlab.example.com:acme/shop.git\n"), 0o644)
	script := filepath.Join(root, "review.sh")
	os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$#\" \"$@\" > \"$CROSS_REVIEW_HOME/args\"\n"), 0o755)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var members []*member
	for _, name := range []string{"Вася", "Петя", "Маша"} {
		home := filepath.Join(root, name)
		store, err := client.OpenStore(home)
		if err != nil {
			t.Fatal(err)
		}
		j, err := client.NewAPI(srv.URL, "").Join(ctx, invite, name, "")
		if err != nil {
			t.Fatal(err)
		}
		p, err := client.NewProject(repoURL, clone, root, script+" review <LINK>")
		if err != nil {
			t.Fatal(err)
		}
		store.Update(func(c *client.Config) error {
			c.Broker, c.Key, c.Name, c.Projects = srv.URL, j.Key, j.Name, []client.Project{p}
			return nil
		})
		n := &notes{}
		a := &client.Agent{Store: store, Notify: n, LogDir: filepath.Join(home, "logs"), Version: "e2e"}
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
	if _, _, err := vasya.agent.RequestReview(ctx, "https://gitlab.example.com/acme/other/-/merge_requests/1"); err == nil || !strings.Contains(err.Error(), "не подключён") {
		t.Fatal("чужой проект:", err)
	}
}
