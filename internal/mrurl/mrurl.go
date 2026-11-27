// Package mrurl разбирает ссылки на merge request и адреса git-remote.
//
// Ссылка на MR приходит от брокера и уходит в команду ревью аргументом процесса, поэтому разбор
// строгий: только https, только безопасные символы, и наружу отдаётся ссылка, собранная заново из
// разобранных частей, а не исходная строка.
package mrurl

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Repo — репозиторий GitLab: хост и путь группы и проекта, в нижнем регистре.
type Repo struct {
	Host string
	Path string
}

// String — ключ репо вида host/group/project.
func (r Repo) String() string { return r.Host + "/" + r.Path }

// MR — merge request репозитория.
type MR struct {
	Repo Repo
	IID  int
}

// String — каноничная ссылка на MR.
func (m MR) String() string {
	return "https://" + m.Repo.Host + "/" + m.Repo.Path + "/-/merge_requests/" + strconv.Itoa(m.IID)
}

// Short — короткое имя для уведомлений: project!iid.
func (m MR) Short() string {
	p := m.Repo.Path
	if i := strings.LastIndex(p, "/"); i >= 0 {
		p = p[i+1:]
	}
	return p + "!" + strconv.Itoa(m.IID)
}

var (
	hostRe    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
	segmentRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)
	iidRe     = regexp.MustCompile(`^[1-9][0-9]{0,9}$`)
	safeRe    = regexp.MustCompile(`^[A-Za-z0-9._/:-]+$`)
)

// ParseMR принимает только каноничную ссылку: https://host/group/project/-/merge_requests/N без
// порта, логина, query, fragment, кодирования и лишних сегментов. Так клиент проверяет то, что
// прислал брокер.
func ParseMR(s string) (MR, error) {
	if !safeRe.MatchString(s) {
		return MR{}, errors.New("в ссылке недопустимые символы")
	}
	u, err := url.Parse(s)
	if err != nil {
		return MR{}, fmt.Errorf("ссылка не разбирается: %w", err)
	}
	if u.Scheme != "https" || u.User != nil || u.Port() != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return MR{}, errors.New("нужна ссылка https://хост/группа/проект/-/merge_requests/N")
	}
	host := u.Hostname()
	if host != strings.ToLower(host) || !hostRe.MatchString(host) {
		return MR{}, errors.New("недопустимый хост в ссылке")
	}
	repoPath, iid, ok := strings.Cut(strings.TrimPrefix(u.Path, "/"), "/-/merge_requests/")
	if !ok {
		return MR{}, errors.New("это не ссылка на merge request")
	}
	if !iidRe.MatchString(iid) {
		return MR{}, errors.New("номер MR должен быть числом")
	}
	path, err := cleanPath(repoPath)
	if err != nil {
		return MR{}, err
	}
	n, _ := strconv.Atoi(iid)
	m := MR{Repo: Repo{Host: host, Path: path}, IID: n}
	if m.String() != s {
		return MR{}, errors.New("ссылка не каноничная")
	}
	return m, nil
}

// NormalizeMR — для ввода человеком: убирает пробелы, query, fragment и хвосты вроде /diffs, приводит
// хост и путь к нижнему регистру и возвращает каноничный MR.
func NormalizeMR(s string) (MR, error) {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	const marker = "/-/merge_requests/"
	i := strings.Index(s, marker)
	if i < 0 {
		return MR{}, errors.New("это не ссылка на merge request")
	}
	rest := s[i+len(marker):]
	if j := strings.Index(rest, "/"); j >= 0 {
		rest = rest[:j]
	}
	return ParseMR(strings.ToLower(s[:i]) + marker + rest)
}

// ParseRemote разбирает адрес репо в любом виде: https://host/g/p(.git), git@host:g/p.git,
// ssh://git@host:2222/g/p.git, git://host/g/p. Логин, порт и .git отбрасываются.
func ParseRemote(s string) (Repo, error) {
	s = strings.TrimSpace(s)
	var host, path string
	switch {
	case strings.Contains(s, "://"):
		u, err := url.Parse(s)
		if err != nil {
			return Repo{}, fmt.Errorf("адрес не разбирается: %w", err)
		}
		switch u.Scheme {
		case "https", "http", "ssh", "git", "git+ssh":
		default:
			return Repo{}, fmt.Errorf("неизвестная схема %q", u.Scheme)
		}
		host, path = u.Hostname(), u.Path
	default:
		// scp-вид: [user@]host:group/project.git
		at := strings.LastIndex(s, "@")
		hp := s[at+1:]
		h, p, ok := strings.Cut(hp, ":")
		if !ok || strings.Contains(h, "/") {
			return Repo{}, errors.New("адрес не похож на git-remote")
		}
		host, path = h, p
	}
	host = strings.ToLower(host)
	if !hostRe.MatchString(host) {
		return Repo{}, errors.New("недопустимый хост")
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if i := strings.Index(path, "/-/"); i >= 0 {
		path = path[:i]
	}
	clean, err := cleanPath(path)
	if err != nil {
		return Repo{}, err
	}
	return Repo{Host: host, Path: clean}, nil
}

// ParseRepoKey разбирает ключ host/group/project, как его хранит брокер.
func ParseRepoKey(s string) (Repo, error) {
	host, path, ok := strings.Cut(s, "/")
	if !ok || host != strings.ToLower(host) || !hostRe.MatchString(host) {
		return Repo{}, errors.New("ключ репо должен быть вида host/group/project")
	}
	clean, err := cleanPath(path)
	if err != nil {
		return Repo{}, err
	}
	if clean != path {
		return Repo{}, errors.New("ключ репо должен быть в нижнем регистре")
	}
	return Repo{Host: host, Path: clean}, nil
}

func cleanPath(p string) (string, error) {
	segs := strings.Split(p, "/")
	if len(segs) < 2 {
		return "", errors.New("нужен путь вида группа/проект")
	}
	for _, s := range segs {
		if !segmentRe.MatchString(s) || strings.HasSuffix(s, ".") || strings.Contains(s, "..") {
			return "", fmt.Errorf("недопустимый сегмент пути %q", s)
		}
	}
	return strings.ToLower(p), nil
}
