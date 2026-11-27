// Package client — клиент cross-review: настройки участника, запуск команды ревью и связь с брокером.
package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/goldquality/cross-review/internal/mrurl"
)

// SettingsFile — имя файла настроек участника.
const SettingsFile = "cross-review.yaml"

// Project — репо, чьи MR участник ревьюит: где запускать команду и какую.
type Project struct {
	Repo    string   // host/group/project
	Workdir string   // папка запуска команды
	Command []string // аргументы с <LINK>
}

// Settings — проверенное содержимое файла настроек.
type Settings struct {
	Broker   string
	Invite   string
	Name     string
	GitLab   string
	Projects []Project
}

// Config — настройки вместе с состоянием клиента: всё, что нужно агенту и трею.
type Config struct {
	Settings
	Path        string // файл настроек
	Err         string // что не так с файлом; пока ошибка не исправлена, действуют последние исправные настройки
	Rev         int    // растёт при каждом успешном перечитывании настроек
	Key         string // личный ключ брокера из настроек; пусто — ещё не подключён
	PausedUntil int64
}

// state — то, что клиент пишет сам. Файл настроек пишет только человек.
type state struct {
	Broker      string `json:"broker"` // брокер, которому принадлежит ключ
	Key         string `json:"key"`
	Name        string `json:"name"` // имя, под которым брокер знает участника
	PausedUntil int64  `json:"paused_until"`
}

// Dir — папка клиента: состояние, логи, замок. CROSS_REVIEW_HOME переопределяет её (два клиента на
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

// SettingsPath — файл настроек: в домашней папке, чтобы его было легко найти и положить туда
// присланный файл; с CROSS_REVIEW_HOME — в ней.
func SettingsPath() (string, error) {
	if d := os.Getenv("CROSS_REVIEW_HOME"); d != "" {
		return filepath.Join(d, SettingsFile), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, SettingsFile), nil
}

type fileProject struct {
	Repo    string `yaml:"repo"`
	Workdir string `yaml:"workdir"`
	Command string `yaml:"command"`
}

type fileSettings struct {
	Broker   string        `yaml:"broker"`
	Invite   string        `yaml:"invite"`
	Name     string        `yaml:"name"`
	GitLab   string        `yaml:"gitlab"`
	Command  string        `yaml:"command"`
	Workdir  string        `yaml:"workdir"`
	Projects []fileProject `yaml:"projects"`
}

// ParseSettings проверяет файл настроек. command и workdir верхнего уровня действуют для всех
// проектов, у которых не указаны свои.
func ParseSettings(b []byte) (Settings, error) {
	var f fileSettings
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		if errors.Is(err, io.EOF) {
			return Settings{}, errors.New("файл пуст")
		}
		return Settings{}, yamlError(err)
	}
	var s Settings
	var err error
	if s.Broker, err = CheckBrokerURL(f.Broker); err != nil {
		return Settings{}, fmt.Errorf("broker: %w", err)
	}
	s.Invite = strings.TrimSpace(f.Invite)
	s.GitLab = strings.TrimSpace(f.GitLab)
	if s.Name = strings.TrimSpace(f.Name); s.Name == "" {
		return Settings{}, errors.New("name: впиши, как тебя показывать команде")
	}
	if len(f.Projects) == 0 {
		return Settings{}, errors.New("projects: нет ни одного репо")
	}
	seen := map[string]bool{}
	for i, fp := range f.Projects {
		repo, err := mrurl.ParseRemote(fp.Repo)
		if err != nil {
			return Settings{}, fmt.Errorf("projects[%d].repo: %w", i+1, err)
		}
		key := repo.String()
		if seen[key] {
			return Settings{}, fmt.Errorf("projects: %s указан дважды", key)
		}
		seen[key] = true
		workdir := firstNonEmpty(fp.Workdir, f.Workdir)
		if workdir == "" {
			return Settings{}, fmt.Errorf("%s: нет workdir ни у проекта, ни общего", key)
		}
		if workdir, err = absDir(workdir); err != nil {
			return Settings{}, fmt.Errorf("%s: workdir: %w", key, err)
		}
		command := firstNonEmpty(fp.Command, f.Command)
		if command == "" {
			return Settings{}, fmt.Errorf("%s: нет command ни у проекта, ни общей", key)
		}
		argv, err := ParseCommand(command)
		if err != nil {
			return Settings{}, fmt.Errorf("%s: command: %w", key, err)
		}
		s.Projects = append(s.Projects, Project{Repo: key, Workdir: workdir, Command: argv})
	}
	return s, nil
}

var unknownField = regexp.MustCompile(`^line (\d+): field (\S+) not found in type \S+$`)

// yamlError — ошибка разбора словами человека: строка и что не так.
func yamlError(err error) error {
	var te *yaml.TypeError
	if !errors.As(err, &te) {
		return errors.New(strings.Replace(err.Error(), "yaml: line ", "строка ", 1))
	}
	msgs := make([]string, 0, len(te.Errors))
	for _, e := range te.Errors {
		e = unknownField.ReplaceAllString(e, "line $1: неизвестное поле $2")
		msgs = append(msgs, strings.Replace(e, "line ", "строка ", 1))
	}
	return errors.New(strings.Join(msgs, "; "))
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

// Store — настройки из файла и состояние клиента. Оба файла перечитываются, когда их поменяли
// со стороны: настройки правит человек, паузу может поставить CLI при запущенном трее.
type Store struct {
	mu        sync.Mutex
	path      string
	statePath string

	set    Settings // последние исправные
	setErr string
	setFP  fileStamp
	rev    int

	st   state
	stFP fileStamp
}

type fileStamp struct {
	mtime time.Time
	size  int64
}

func stamp(path string) (fileStamp, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}, err
	}
	return fileStamp{mtime: fi.ModTime(), size: fi.Size()}, nil
}

// OpenStore открывает настройки settingsPath и состояние в папке клиента dir.
func OpenStore(dir, settingsPath string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: settingsPath, statePath: filepath.Join(dir, "state.json")}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadSettings()
	if err := s.reloadState(); err != nil {
		return nil, err
	}
	return s, nil
}

// Path — файл настроек.
func (s *Store) Path() string { return s.path }

func (s *Store) reloadSettings() {
	fp, err := stamp(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.setErr, s.setFP = "нет файла настроек "+s.path, fileStamp{}
		return
	}
	if err != nil {
		s.setErr = err.Error()
		return
	}
	if fp == s.setFP {
		return
	}
	s.setFP = fp
	b, err := os.ReadFile(s.path)
	if err != nil {
		s.setErr = err.Error()
		return
	}
	set, err := ParseSettings(b)
	if err != nil {
		s.setErr = err.Error()
		return
	}
	s.set, s.setErr = set, ""
	s.rev++
}

func (s *Store) reloadState() error {
	fp, err := stamp(s.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fp == s.stFP {
		return nil
	}
	b, err := os.ReadFile(s.statePath)
	if err != nil {
		return err
	}
	var st state
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("состояние %s испорчено: %w", s.statePath, err)
	}
	s.st, s.stFP = st, fp
	return nil
}

// Get — настройки и состояние сейчас.
func (s *Store) Get() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadSettings()
	s.reloadState()
	c := Config{Settings: s.set, Path: s.path, Err: s.setErr, Rev: s.rev, PausedUntil: s.st.PausedUntil}
	c.Projects = append([]Project(nil), s.set.Projects...)
	if c.Broker != "" && s.st.Broker == c.Broker {
		c.Key = s.st.Key
		if s.st.Name != "" {
			c.Name = s.st.Name
		}
	}
	return c
}

// updateState меняет состояние и сохраняет его атомарно с правами 0600: в нём личный ключ.
func (s *Store) updateState(fn func(*state)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadState(); err != nil {
		return err
	}
	next := s.st
	fn(&next)
	b, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.statePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.statePath); err != nil {
		return err
	}
	s.st = next
	if fp, err := stamp(s.statePath); err == nil {
		s.stFP = fp
	}
	return nil
}

// EnsureSettings создаёт файл настроек из шаблона, если его нет. created — файл только что создан.
func (s *Store) EnsureSettings() (created bool, err error) {
	f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	_, err = f.WriteString(SettingsTemplate)
	return err == nil, err
}

// SettingsTemplate — файл настроек нового участника. Готовый файл для команды обычно присылает
// администратор брокера.
const SettingsTemplate = `# Настройки cross-review. Клиент перечитывает файл сам после сохранения.
# Готовый файл для команды обычно присылает администратор брокера: остаётся вписать имя.

# Адрес брокера и приглашение от его администратора. Приглашение нужно только при первом
# подключении: клиент обменяет его на личный ключ.
broker: https://review.example.com
invite: inv_…

# Как тебя видит команда. Задаётся при первом подключении.
name: ""

# Вход в GitLab, необязательно.
gitlab: ""

# Команда ревью и папка, где её запускать, — для всех проектов, у которых не указаны свои.
# Вместо <LINK> подставится ссылка на MR. Шелл не участвует: | и && не работают.
command: ./dev review <LINK>
workdir: ~/projects/devtools

# Репо, чьи MR ты ревьюишь и в которых просишь ревью сам.
projects:
  - repo: https://gitlab.example.com/acme/api
  - repo: https://gitlab.example.com/acme/web
    # workdir: ~/projects/web
    # command: ./review.sh <LINK>
`

// ProjectFor — проект репо или nil.
func (c Config) ProjectFor(repo string) *Project {
	for i := range c.Projects {
		if c.Projects[i].Repo == repo {
			return &c.Projects[i]
		}
	}
	return nil
}

// Repos — ключи репо для брокера.
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

// absDir — абсолютный путь существующей папки; ~ — домашняя папка.
func absDir(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, p[1:])
	}
	p, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(p)
	if err != nil {
		return "", fmt.Errorf("нет папки %s", p)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s — не папка", p)
	}
	return p, nil
}
