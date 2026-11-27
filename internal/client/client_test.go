package client

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/goldquality/cross-review/internal/mrurl"
)

func TestParseCommand(t *testing.T) {
	cases := map[string][]string{
		"./dev review <LINK>":                       {"./dev", "review", "<LINK>"},
		`  claude -p "/review-mr <LINK>"  `:         {"claude", "-p", "/review-mr <LINK>"},
		`C:\tools\review.exe --url=<LINK>`:          {`C:\tools\review.exe`, "--url=<LINK>"},
		`'/Applications/My App/rev' '<LINK>' "a b"`: {"/Applications/My App/rev", "<LINK>", "a b"},
	}
	for in, want := range cases {
		got, err := ParseCommand(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%q: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "   ", "./dev review", "<LINK> x", `./dev "review <LINK>`, "./dev\nreview <LINK>"} {
		if got, err := ParseCommand(bad); err == nil {
			t.Fatalf("%q принят: %q", bad, got)
		}
	}
}

func TestSubstituteKeepsLinkInOneArg(t *testing.T) {
	argv := []string{"sh", "-c", "echo", "--url=<LINK>", "<LINK>"}
	got := Substitute(argv, "https://h.x/g/p/-/merge_requests/1")
	want := []string{"sh", "-c", "echo", "--url=https://h.x/g/p/-/merge_requests/1", "https://h.x/g/p/-/merge_requests/1"}
	if !reflect.DeepEqual(got, want) || argv[4] != "<LINK>" {
		t.Fatal(got)
	}
}

func TestCheckBrokerURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://review.example.com":  "https://review.example.com",
		"https://review.example.com/": "https://review.example.com",
		"http://localhost:18080":      "http://localhost:18080",
		"http://127.0.0.1:8080/":      "http://127.0.0.1:8080",
	} {
		got, err := CheckBrokerURL(in)
		if err != nil || got != want {
			t.Fatalf("%q: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"http://review.example.com", "review.example.com", "https://u:p@x.com", "https://x.com/path", "ftp://x.com"} {
		if _, err := CheckBrokerURL(bad); err == nil {
			t.Fatalf("%q принят", bad)
		}
	}
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseSettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	devtools := filepath.Join(home, "devtools")
	web := filepath.Join(home, "web")
	os.MkdirAll(devtools, 0o755)
	os.MkdirAll(web, 0o755)
	s, err := ParseSettings([]byte(`
broker: https://review.example.com/
invite: inv_abc
name: "  Вася "
command: ./dev review <LINK>
workdir: ~/devtools
projects:
  - repo: git@gitlab.example.com:acme/api.git
  - repo: https://gitlab.example.com/acme/web
    workdir: ` + web + `
    command: ./review.sh --url=<LINK>
`))
	if err != nil {
		t.Fatal(err)
	}
	want := Settings{Broker: "https://review.example.com", Invite: "inv_abc", Name: "Вася", Projects: []Project{
		{Repo: "gitlab.example.com/acme/api", Workdir: devtools, Command: []string{"./dev", "review", "<LINK>"}},
		{Repo: "gitlab.example.com/acme/web", Workdir: web, Command: []string{"./review.sh", "--url=<LINK>"}},
	}}
	if !reflect.DeepEqual(s, want) {
		t.Fatalf("\n got %+v\nwant %+v", s, want)
	}

	base := "broker: https://review.example.com\nname: Вася\ncommand: ./dev review <LINK>\nworkdir: " + devtools + "\n"
	bad := map[string]string{
		"": "файл пуст",
		"name: Вася\nprojects: [{repo: https://gitlab.example.com/a/b}]\n":                                          "broker:",
		"broker: http://review.example.com\nname: x\n":                                                              "broker:",
		strings.Replace(base, "name: Вася", "name: ''", 1) + "projects: [{repo: https://gitlab.example.com/a/b}]\n": "name:",
		base + "comand: x\n":   "строка 5: неизвестное поле comand",
		base + "projects: [\n": "строка",
		base:                   "projects:",
		base + "projects: [{repo: 'https://gitlab.example.com/a/b;id'}]\n":                                                     "projects[1].repo",
		base + "projects: [{repo: https://gitlab.example.com/a/b}, {repo: git@gitlab.example.com:a/b.git}]\n":                  "дважды",
		base + "projects: [{repo: https://gitlab.example.com/a/b, command: ./dev review}]\n":                                   "command:",
		base + "projects: [{repo: https://gitlab.example.com/a/b, workdir: " + filepath.Join(home, "nope") + "}]\n":            "нет папки",
		"broker: https://review.example.com\nname: x\ncommand: x <LINK>\nprojects: [{repo: https://gitlab.example.com/a/b}]\n": "нет workdir",
	}
	for text, sub := range bad {
		if _, err := ParseSettings([]byte(text)); err == nil || !strings.Contains(err.Error(), sub) {
			t.Errorf("%q: ждал ошибку с %q, получил %v", text, sub, err)
		}
	}
}

func TestSettingsTemplateParses(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir, filepath.Join(dir, SettingsFile))
	if err != nil {
		t.Fatal(err)
	}
	if created, err := s.EnsureSettings(); err != nil || !created {
		t.Fatal(created, err)
	}
	if created, _ := s.EnsureSettings(); created {
		t.Fatal("существующий файл перезаписан")
	}
	// Шаблон — рабочий файл, в котором осталось вписать своё: имя и папку.
	text := strings.Replace(SettingsTemplate, `name: ""`, "name: Вася", 1)
	text = strings.Replace(text, "~/projects/devtools", dir, 1)
	if _, err := ParseSettings([]byte(text)); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseSettings([]byte(SettingsTemplate)); err == nil || !strings.Contains(err.Error(), "name:") {
		t.Fatal("шаблон без имени принят:", err)
	}
}

func TestStoreReloadsSettingsAndKeepsLastGood(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, SettingsFile)
	s, err := OpenStore(dir, path)
	if err != nil {
		t.Fatal(err)
	}
	if c := s.Get(); !strings.Contains(c.Err, "нет файла настроек") || c.Broker != "" {
		t.Fatal(c)
	}
	good := "broker: https://review.example.com\nname: Вася\ncommand: x <LINK>\nworkdir: " + dir + "\nprojects:\n  - repo: https://gitlab.example.com/a/b\n"
	writeFile(t, path, good)
	c := s.Get()
	if c.Err != "" || len(c.Projects) != 1 || c.Rev != 1 {
		t.Fatal(c)
	}
	writeFile(t, path, strings.Replace(good, "name: Вася", "name: ''", 1))
	if c := s.Get(); !strings.Contains(c.Err, "name:") || len(c.Projects) != 1 || c.Rev != 1 {
		t.Fatal("ошибка в файле должна оставить прежние настройки:", c)
	}
	writeFile(t, path, good+"  - repo: https://gitlab.example.com/a/c\n")
	if c := s.Get(); c.Err != "" || len(c.Projects) != 2 || c.Rev != 2 {
		t.Fatal(c)
	}
}

func TestKeyBelongsToBroker(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, SettingsFile)
	settings := func(broker string) string {
		return "broker: " + broker + "\nname: Вася\ncommand: x <LINK>\nworkdir: " + dir + "\nprojects:\n  - repo: https://gitlab.example.com/a/b\n"
	}
	writeFile(t, path, settings("https://one.example.com"))
	s, err := OpenStore(dir, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.updateState(func(st *state) { st.Broker, st.Key, st.Name = "https://one.example.com", "crk_x", "Вася" }); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(filepath.Join(dir, "state.json"))
		if fi.Mode().Perm() != 0o600 {
			t.Fatal("ключ лежит не 0600:", fi.Mode())
		}
	}
	s2, _ := OpenStore(dir, path)
	if s2.Get().Key != "crk_x" {
		t.Fatal("ключ не сохранился:", s2.Get())
	}
	// Другой брокер в настройках — ключ прежнего к нему не подходит, нужно подключиться заново.
	writeFile(t, path, settings("https://another.example.com"))
	if k := s2.Get().Key; k != "" {
		t.Fatal("ключ чужого брокера:", k)
	}
}

func TestRunPassesLinkAsOneArgument(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("скрипт на sh")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "review.sh")
	os.WriteFile(script, []byte("#!/bin/sh\necho \"argc=$#\"\nfor a in \"$@\"; do echo \"arg=[$a]\"; done\npwd\nexit 7\n"), 0o755)
	mr, _ := mrurl.ParseMR("https://gitlab.example.com/acme/shop/-/merge_requests/9")
	p := Project{Repo: mr.Repo.String(), Workdir: dir, Command: []string{"./review.sh", "review", "<LINK>"}}
	res, err := Run(context.Background(), p, mr, filepath.Join(dir, "logs"), os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 7 {
		t.Fatal(res)
	}
	if !strings.Contains(res.Tail, "argc=2") || !strings.Contains(res.Tail, "arg=["+mr.String()+"]") || !strings.Contains(res.Tail, dir) {
		t.Fatal(res.Tail)
	}
	log, _ := os.ReadFile(res.LogPath)
	if !strings.Contains(string(log), "код выхода 7") {
		t.Fatal(string(log))
	}
	other, _ := mrurl.ParseMR("https://gitlab.example.com/acme/other/-/merge_requests/1")
	if _, err := Run(context.Background(), p, other, dir, nil); err == nil {
		t.Fatal("MR чужого репо запущен")
	}
}

func TestRunStopKillsTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("скрипт на sh")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "slow.sh")
	os.WriteFile(script, []byte("#!/bin/sh\nsleep 30 &\nwait\n"), 0o755)
	mr, _ := mrurl.ParseMR("https://gitlab.example.com/acme/shop/-/merge_requests/9")
	p := Project{Repo: mr.Repo.String(), Workdir: dir, Command: []string{"./slow.sh", "<LINK>"}}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	start := time.Now()
	res, err := Run(ctx, p, mr, dir, os.Environ())
	if err != nil || res.ExitCode != -1 || time.Since(start) > 5*time.Second || !strings.Contains(res.Tail, "остановлено") {
		t.Fatal(res, err, time.Since(start))
	}
}

func TestNewerVersion(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"v0.1.3", "v0.1.2", true},
		{"v0.1.10", "v0.1.9", true},
		{"v0.2.0", "v0.1.9", true},
		{"v1.0.0", "v0.9.9", true},
		{"v0.1.2", "v0.1.2", false},
		{"v0.1.1", "v0.1.2", false},
		{"v0.1.3", "dev", false},
		{"dev", "v0.1.2", false},
		{"", "v0.1.2", false},
		{"v0.1.3-rc1", "v0.1.2", false},
		{"v0.01.3", "v0.1.2", false},
		{"0.1.3", "v0.1.2", false},
	}
	for _, c := range cases {
		if got := NewerVersion(c.latest, c.current); got != c.want {
			t.Errorf("NewerVersion(%q, %q) = %v", c.latest, c.current, got)
		}
	}
}
