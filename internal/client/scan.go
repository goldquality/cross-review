package client

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/goldquality/cross-review/internal/mrurl"
)

// Папки, в которых клонов не ищем: зависимости, кэши, системные.
var skipDirs = map[string]bool{
	"node_modules": true, "vendor": true, "Library": true, "AppData": true, "Applications": true,
	"Pictures": true, "Music": true, "Movies": true, "Videos": true, "go": true, "venv": true,
	"__pycache__": true, "target": true, "build": true, "dist": true, "OneDrive": true,
}

// ScanDepth — на сколько уровней от домашней папки ищем клоны.
const ScanDepth = 5

// Scan ищет клоны репо want под root на глубину depth и сообщает каждый найденный путь. Скрытые
// папки и зависимости пропускаются, символические ссылки не обходятся.
func Scan(ctx context.Context, root string, depth int, want mrurl.Repo, found func(string)) error {
	return scanDir(ctx, root, depth, want.String(), found)
}

func scanDir(ctx context.Context, dir string, depth int, want string, found func(string)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil // нет доступа — идём дальше
	}
	for _, e := range entries {
		if e.Name() == ".git" {
			if remotes, err := RepoRemotes(dir); err == nil {
				for _, r := range remotes {
					if r == want {
						found(dir)
						break
					}
				}
			}
			break
		}
	}
	if depth == 0 {
		return nil
	}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || strings.HasPrefix(name, ".") || skipDirs[name] {
			continue
		}
		if err := scanDir(ctx, filepath.Join(dir, name), depth-1, want, found); err != nil {
			return err
		}
	}
	return nil
}

// RepoRemotes — ключи host/group/project всех remote клона. Читает .git/config напрямую, без git;
// понимает worktree и submodule, у которых .git — файл со ссылкой.
func RepoRemotes(dir string) ([]string, error) {
	gitDir := filepath.Join(dir, ".git")
	fi, err := os.Stat(gitDir)
	if err != nil {
		return nil, errors.New("это не git-клон")
	}
	if !fi.IsDir() {
		b, err := os.ReadFile(gitDir)
		if err != nil {
			return nil, err
		}
		ref, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
		if !ok {
			return nil, errors.New("непонятный файл .git")
		}
		gitDir = strings.TrimSpace(ref)
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(dir, gitDir)
		}
		if c, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
			common := strings.TrimSpace(string(c))
			if !filepath.IsAbs(common) {
				common = filepath.Join(gitDir, common)
			}
			gitDir = common
		}
	}
	f, err := os.Open(filepath.Join(gitDir, "config"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	inRemote := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			inRemote = strings.HasPrefix(line, "[remote ")
			continue
		}
		if !inRemote {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != "url" {
			continue
		}
		if r, err := mrurl.ParseRemote(strings.Trim(strings.TrimSpace(v), `"`)); err == nil {
			out = append(out, r.String())
		}
	}
	return out, sc.Err()
}
