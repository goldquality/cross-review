// Package client — клиент cross-review: проекты участника, сканер клонов, запуск команды ревью и
// связь с брокером.
package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/goldquality/cross-review/internal/mrurl"
)

// Project — подключённый репо: где клон, где запускать команду и какая команда.
type Project struct {
	Repo    string   `json:"repo"`    // host/group/project
	Clone   string   `json:"clone"`   // путь к клону
	Workdir string   `json:"workdir"` // папка запуска команды
	Command []string `json:"command"` // аргументы с <LINK>
}

// Config — всё, что клиент хранит на машине участника. Команды отсюда никуда не уходят.
type Config struct {
	Broker      string    `json:"broker"`
	Key         string    `json:"key"`
	Name        string    `json:"name"`
	Projects    []Project `json:"projects"`
	PausedUntil int64     `json:"paused_until"`
}

// Dir — папка клиента: конфиг, логи, замок. CROSS_REVIEW_HOME переопределяет её (два клиента на
// одной машине, тесты).
func Dir() (string, error) {
	if d := os.Getenv("CROSS_REVIEW_HOME"); d != "" {
		return d, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "cross-review"), nil
}

// Store — конфиг с замком: трей и агент меняют его из разных горутин.
type Store struct {
	mu    sync.Mutex
	path  string
	cfg   Config
	mtime time.Time
}

// OpenStore читает конфиг или начинает пустой.
func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "config.json")}
	if err := s.reload(true); err != nil {
		return nil, err
	}
	return s, nil
}

// reload перечитывает файл, если его поменяли со стороны: CLI при запущенном трее.
func (s *Store) reload(force bool) error {
	fi, err := os.Stat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !force && fi.ModTime().Equal(s.mtime) {
		return nil
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return fmt.Errorf("конфиг %s испорчен: %w", s.path, err)
	}
	s.cfg, s.mtime = c, fi.ModTime()
	return nil
}

// Get — копия конфига.
func (s *Store) Get() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reload(false)
	c := s.cfg
	c.Projects = append([]Project(nil), s.cfg.Projects...)
	return c
}

// Update меняет конфиг и сохраняет его атомарно с правами 0600.
func (s *Store) Update(fn func(*Config) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reload(false); err != nil {
		return err
	}
	next := s.cfg
	next.Projects = append([]Project(nil), s.cfg.Projects...)
	if err := fn(&next); err != nil {
		return err
	}
	b, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.cfg = next
	if fi, err := os.Stat(s.path); err == nil {
		s.mtime = fi.ModTime()
	}
	return nil
}

// ProjectFor — подключённый проект репо или nil.
func (c Config) ProjectFor(repo string) *Project {
	for i := range c.Projects {
		if c.Projects[i].Repo == repo {
			return &c.Projects[i]
		}
	}
	return nil
}

// Repos — ключи подключённых репо для брокера.
func (c Config) Repos() []string {
	out := make([]string, 0, len(c.Projects))
	for _, p := range c.Projects {
		out = append(out, p.Repo)
	}
	return out
}

// Paused — клиент на паузе сейчас.
func (c Config) Paused(now time.Time) bool { return c.PausedUntil > now.Unix() }

// CheckBrokerURL пропускает https и http только на localhost (для проверки у себя).
func CheckBrokerURL(s string) (string, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "/")
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", errors.New("адрес брокера — https://домен")
	}
	host := u.Hostname()
	local := host == "localhost" || host == "127.0.0.1" || host == "::1"
	if u.Scheme != "https" && !(u.Scheme == "http" && local) {
		return "", errors.New("адрес брокера должен быть https://")
	}
	return u.Scheme + "://" + u.Host, nil
}

// NewProject проверяет и собирает проект из ввода участника.
func NewProject(repoURL, clone, workdir, command string) (Project, error) {
	repo, err := mrurl.ParseRemote(repoURL)
	if err != nil {
		return Project{}, fmt.Errorf("адрес репо: %w", err)
	}
	clone, err = absDir(clone)
	if err != nil {
		return Project{}, fmt.Errorf("клон: %w", err)
	}
	remotes, err := RepoRemotes(clone)
	if err != nil {
		return Project{}, fmt.Errorf("клон %s: %w", clone, err)
	}
	match := false
	for _, r := range remotes {
		if r == repo.String() {
			match = true
		}
	}
	if !match {
		return Project{}, fmt.Errorf("у клона %s нет remote на %s", clone, repo)
	}
	if strings.TrimSpace(workdir) == "" {
		workdir = clone
	}
	workdir, err = absDir(workdir)
	if err != nil {
		return Project{}, fmt.Errorf("папка запуска: %w", err)
	}
	argv, err := ParseCommand(command)
	if err != nil {
		return Project{}, err
	}
	return Project{Repo: repo.String(), Clone: clone, Workdir: workdir, Command: argv}, nil
}

func absDir(p string) (string, error) {
	p = strings.TrimSpace(p)
	if strings.HasPrefix(p, "~/") || p == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	p, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s — не папка", p)
	}
	return p, nil
}
