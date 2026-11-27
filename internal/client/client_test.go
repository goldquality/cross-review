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

// fakeClone создаёт папку с .git/config и remote origin.
func fakeClone(t *testing.T, dir, remote string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "[core]\n\tbare = false\n[remote \"origin\"]\n\turl = " + remote + "\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n[branch \"main\"]\n\tremote = origin\n"
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanFindsClones(t *testing.T) {
	home := t.TempDir()
	fakeClone(t, filepath.Join(home, "projects", "devtools"), "ssh://git@gitlab.example.com:2222/acme/devtools.git")
	fakeClone(t, filepath.Join(home, "projects", "devtools", "sources", "api"), "ssh://git@gitlab.example.com:2222/acme/api.git")
	fakeClone(t, filepath.Join(home, "work", "api-copy"), "https://gitlab.example.com/acme/api.git")
	fakeClone(t, filepath.Join(home, "projects", "x", "node_modules", "api"), "git@gitlab.example.com:acme/api.git")
	fakeClone(t, filepath.Join(home, ".cache", "api"), "git@gitlab.example.com:acme/api.git")
	fakeClone(t, filepath.Join(home, "a", "b", "c", "d", "e", "f", "deep"), "git@gitlab.example.com:acme/api.git")
	// worktree: .git — файл со ссылкой на основной клон
	wt := filepath.Join(home, "wt", "api-feature")
	os.MkdirAll(wt, 0o755)
	main := filepath.Join(home, "projects", "devtools", "sources", "api", ".git")
	os.MkdirAll(filepath.Join(main, "worktrees", "api-feature"), 0o755)
	os.WriteFile(filepath.Join(main, "worktrees", "api-feature", "commondir"), []byte("../.."), 0o644)
	os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+filepath.Join(main, "worktrees", "api-feature")+"\n"), 0o644)

	want, _ := mrurl.ParseRemote("https://gitlab.example.com/acme/api")
	var found []string
	if err := Scan(context.Background(), home, ScanDepth, want, func(p string) { found = append(found, strings.TrimPrefix(p, home)) }); err != nil {
		t.Fatal(err)
	}
	sep := string(filepath.Separator)
	expect := []string{
		sep + filepath.Join("projects", "devtools", "sources", "api"),
		sep + filepath.Join("work", "api-copy"),
		sep + filepath.Join("wt", "api-feature"),
	}
	got := map[string]bool{}
	for _, f := range found {
		got[f] = true
	}
	if len(found) != len(expect) {
		t.Fatal(found)
	}
	for _, e := range expect {
		if !got[e] {
			t.Fatal("не найден", e, found)
		}
	}
}

func TestNewProject(t *testing.T) {
	dir := t.TempDir()
	clone := filepath.Join(dir, "api")
	fakeClone(t, clone, "git@gitlab.example.com:acme/api.git")
	p, err := NewProject("https://gitlab.example.com/acme/api", clone, dir, "./dev review <LINK>")
	if err != nil {
		t.Fatal(err)
	}
	if p.Repo != "gitlab.example.com/acme/api" || p.Workdir != dir || p.Clone != clone || p.Command[2] != "<LINK>" {
		t.Fatal(p)
	}
	if _, err := NewProject("https://gitlab.example.com/acme/web", clone, "", "./dev review <LINK>"); err == nil {
		t.Fatal("клон чужого репо принят")
	}
	if _, err := NewProject("https://gitlab.example.com/acme/api", clone, "", "./dev review"); err == nil {
		t.Fatal("команда без <LINK> принята")
	}
	if _, err := NewProject("https://gitlab.example.com/acme/api", filepath.Join(dir, "nope"), "", "x <LINK>"); err == nil {
		t.Fatal("несуществующий клон принят")
	}
}

func TestStoreSavesPrivately(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(c *Config) error { c.Key = "crk_x"; c.Broker = "https://b.example.com"; return nil }); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(filepath.Join(dir, "config.json"))
		if fi.Mode().Perm() != 0o600 {
			t.Fatal(fi.Mode())
		}
	}
	s2, _ := OpenStore(dir)
	if s2.Get().Key != "crk_x" {
		t.Fatal(s2.Get())
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
