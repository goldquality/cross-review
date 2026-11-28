package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goldquality/cross-review/internal/mrurl"
)

func TestClean(t *testing.T) {
	cases := map[string]string{
		"shop!9: Вася взял ревью":            "shop!9: Вася взял ревью",
		"done $(iwr https://x/p.ps1|iex)":    "done (iwr https://x/p.ps1|iex)",
		"x\n\"@\nStart-Process calc\n$z=@\"": "x '@ Start-Process calc z=@'",
		"a`$env:USERPROFILE":                 "aenv:USERPROFILE",
		"\x1b[2K\x1b]52;c;aGk=\x07стёр":      "[2K]52;c;aGk=стёр",
		"имя‮текст​":                         "имятекст",
		"<b>жирно</b> & “кавычки”":           "‹b›жирно‹/b› + 'кавычки'",
		"]]><text>": "]]›‹text›",
	}
	for in, want := range cases {
		if got := Clean(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	long := Clean(strings.Repeat("я", 1000))
	if r := []rune(long); len(r) != maxShown+1 || r[maxShown] != '…' {
		t.Fatal("длина:", len(r))
	}
}

// fakeBroker — брокер, который отвечает заданным кодом, пока не исчерпает ответы.
type fakeBroker struct {
	mu     sync.Mutex
	codes  []int
	bodies []string
	hits   atomic.Int32
}

func (f *fakeBroker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.hits.Add(1)
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.bodies = append(f.bodies, string(b))
	code := http.StatusNoContent
	if len(f.codes) > 0 {
		code, f.codes = f.codes[0], f.codes[1:]
	}
	f.mu.Unlock()
	if code >= 400 {
		w.WriteHeader(code)
		w.Write([]byte(`{"error":"нет"}`))
		return
	}
	w.WriteHeader(code)
}

func TestResultRetriedUntilBrokerAnswers(t *testing.T) {
	defer func(b, f time.Duration) { retryBase, retryFor = b, f }(retryBase, retryFor)
	retryBase, retryFor = 10*time.Millisecond, 5*time.Second
	fb := &fakeBroker{codes: []int{502, 503}}
	srv := httptest.NewServer(fb)
	defer srv.Close()
	a := &Agent{}
	a.report(context.Background(), NewAPI(srv.URL, "crk_x"), 7, Result{ExitCode: 0, Tail: "вывод ревью с кусками кода"})
	if fb.hits.Load() != 3 {
		t.Fatal("итог повторяется, пока брокер не ответит:", fb.hits.Load(), lastErr(a))
	}
	var in map[string]any
	json.Unmarshal([]byte(fb.bodies[2]), &in)
	if in["error_tail"] != "" {
		t.Fatal("вывод удачного ревью брокеру не уходит:", in)
	}
	// Отказ брокера не повторяется.
	fb2 := &fakeBroker{codes: []int{422}}
	srv2 := httptest.NewServer(fb2)
	defer srv2.Close()
	a2 := &Agent{}
	a2.report(context.Background(), NewAPI(srv2.URL, "crk_x"), 7, Result{ExitCode: 1, Tail: "упало"})
	json.Unmarshal([]byte(fb2.bodies[0]), &in)
	if fb2.hits.Load() != 1 || !strings.Contains(lastErr(a2), "итог не отправлен") || in["error_tail"] != "упало" {
		t.Fatal(fb2.hits.Load(), lastErr(a2), in)
	}
}

func lastErr(a *Agent) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state.LastError
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	var leaked atomic.Value
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		leaked.Store(r.Header.Get("Authorization") + string(b))
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusPermanentRedirect)
	}))
	defer redirect.Close()
	_, err := NewAPI(redirect.URL, "").Join(context.Background(), "inv_secret", "")
	if err == nil || !strings.Contains(err.Error(), "редиректом 308") {
		t.Fatal(err)
	}
	if _, err := NewAPI(redirect.URL, "crk_secret").Whoami(context.Background()); err == nil {
		t.Fatal("редирект — ошибка")
	}
	if v := leaked.Load(); v != nil {
		t.Fatal("приглашение или ключ ушли по редиректу:", v)
	}
}

func TestRunDoesNotWaitForBackgroundChildren(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh")
	}
	defer func(d time.Duration) { outputGrace = d }(outputGrace)
	outputGrace = 300 * time.Millisecond
	mr, _ := mrurl.ParseMR("https://gitlab.example.com/a/b/-/merge_requests/1")
	dir := t.TempDir()
	p := Project{Repo: "gitlab.example.com/a/b", Workdir: dir, Command: []string{"/bin/sh", "-c", "sleep 3 & echo готово", "<LINK>"}}
	start := time.Now()
	res, err := Run(context.Background(), p, mr, filepath.Join(dir, "logs"), nil)
	if err != nil || res.ExitCode != 0 || time.Since(start) > 2*time.Second {
		t.Fatal("ревью закончилось, фоновый потомок держит вывод — итог сразу и удачный:", res, err, time.Since(start))
	}
	if !strings.Contains(res.Tail, "готово") {
		t.Fatal(res.Tail)
	}
}

type countNotes struct {
	mu   sync.Mutex
	list []string
}

func (c *countNotes) Notify(_, text string) {
	c.mu.Lock()
	c.list = append(c.list, text)
	c.mu.Unlock()
}

func TestBeatLimitsNotificationsAndReportsIgnored(t *testing.T) {
	events := []Event{}
	for i := 0; i < 8; i++ {
		events = append(events, Event{Text: fmt.Sprintf("событие %d", i)})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Beat{Balance: 2, Events: events, Ignored: []string{"evil.example/x/y"}})
	}))
	defer srv.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, SettingsFile)
	writeFile(t, path, "broker: "+srv.URL+"\ncommand: x <LINK>\nworkdir: "+dir+"\nprojects:\n  - repo: https://gitlab.example.com/a/b\n")
	s, err := OpenStore(dir, path)
	if err != nil {
		t.Fatal(err)
	}
	s.updateState(func(st *state) { st.Broker, st.Key = srv.URL, "crk_x" })
	notes := &countNotes{}
	a := &Agent{Store: s, Notify: notes}
	a.beat(context.Background())
	a.beat(context.Background())
	got := strings.Join(notes.list, "\n")
	// За отметку — 4 события и строка «и ещё», о не разрешённом репо — один раз за две отметки.
	if len(notes.list) != 11 || strings.Count(got, "Брокер не разрешает evil.example/x/y") != 1 || strings.Count(got, "И ещё 4") != 2 {
		t.Fatal(len(notes.list), got)
	}
	if st := a.State(); len(st.Ignored) != 1 {
		t.Fatal(st.Ignored)
	}
}
