// Package broker — сервер cross-review: участники, баллы, запросы ревью и задания.
//
// Брокер раздаёт только ссылки на MR. Что запускать, решает клиент участника.
package broker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/goldquality/cross-review/internal/mrurl"
)

// Правила, о которых договорились.
const (
	StartBalance   = 2
	OnlineWindow   = 90 * time.Second
	OfflineAfter   = 3 * time.Minute
	JobTimeout     = 65 * time.Minute // клиент сам убивает команду через 60 минут
	RequestTTL     = 24 * time.Hour
	MaxPause       = 25 * time.Hour
	maxRepos       = 100
	maxErrorTail   = 2000
	maxEventsBatch = 50
)

// Ошибки, которые видит участник.
var (
	ErrBadInvite = errors.New("приглашение не подходит")
	ErrBadKey    = errors.New("ключ не подходит или отозван")
	ErrNameTaken = errors.New("это имя уже занято")
)

// UserError — отказ по правилам, текст показывается участнику.
type UserError struct{ Msg string }

func (e *UserError) Error() string { return e.Msg }

func userErr(format string, a ...any) error { return &UserError{Msg: fmt.Sprintf(format, a...)} }

// Service — вся логика брокера поверх SQLite.
type Service struct {
	db  *sql.DB
	mu  sync.Mutex
	Now func() time.Time
	// Version — версия брокера. Клиенты выходят из того же тега, поэтому это и последняя версия
	// клиента: по ней трей подсказывает обновиться.
	Version string
}

// NewService оборачивает открытую базу.
func NewService(db *sql.DB) *Service {
	return &Service{db: db, Now: time.Now}
}

func (s *Service) now() int64 { return s.Now().Unix() }

// --- участники ----------------------------------------------------------------

var nameRe = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} ._-]*$`)

// nameKey — имя без учёта регистра: NOCASE в SQLite знает только латиницу.
func nameKey(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

func hashKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

func randomToken(prefix string, n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

// Joined — результат входа по приглашению.
type Joined struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Balance int    `json:"balance"`
}

// Join меняет приглашение на личный ключ и начисляет стартовые баллы.
func (s *Service) Join(invite, name string) (Joined, error) {
	name = strings.TrimSpace(name)
	if utf8.RuneCountInString(name) > 40 || !nameRe.MatchString(name) {
		return Joined{}, userErr("имя: буквы, цифры, пробел, точка, дефис, до 40 символов")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	want, err := s.setting("invite")
	if err != nil {
		return Joined{}, err
	}
	if want == "" || subtle.ConstantTimeCompare([]byte(want), []byte(strings.TrimSpace(invite))) != 1 {
		return Joined{}, ErrBadInvite
	}
	key := randomToken("crk_", 32)
	now := s.now()
	tx, err := s.db.Begin()
	if err != nil {
		return Joined{}, err
	}
	defer tx.Rollback()
	var taken int
	if err := tx.QueryRow(`SELECT count(*) FROM members WHERE name_key = ?`, nameKey(name)).Scan(&taken); err != nil {
		return Joined{}, err
	}
	if taken > 0 {
		return Joined{}, ErrNameTaken
	}
	res, err := tx.Exec(`INSERT INTO members(name, name_key, key_hash, created_at) VALUES(?,?,?,?)`, name, nameKey(name), hashKey(key), now)
	if err != nil {
		return Joined{}, err
	}
	id, _ := res.LastInsertId()
	if _, err := tx.Exec(`INSERT INTO ledger(member_id, delta, reason, at) VALUES(?,?,?,?)`, id, StartBalance, "start", now); err != nil {
		return Joined{}, err
	}
	if err := tx.Commit(); err != nil {
		return Joined{}, err
	}
	return Joined{Key: key, Name: name, Balance: StartBalance}, nil
}

// Member — участник, от имени которого пришёл запрос.
type Member struct {
	ID   int64
	Name string
}

// Auth находит участника по личному ключу.
func (s *Service) Auth(key string) (Member, error) {
	if !strings.HasPrefix(key, "crk_") {
		return Member{}, ErrBadKey
	}
	var m Member
	err := s.db.QueryRow(`SELECT id, name FROM members WHERE key_hash = ? AND revoked = 0`, hashKey(key)).Scan(&m.ID, &m.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return Member{}, ErrBadKey
	}
	return m, err
}

// Event — уведомление для трея участника.
type Event struct {
	At   int64  `json:"at"`
	Text string `json:"text"`
}

// Beat — ответ на отметку клиента.
type Beat struct {
	Balance    int     `json:"balance"`
	Events     []Event `json:"events"`
	ServerTime int64   `json:"server_time"`
	Version    string  `json:"version"`
}

// Heartbeat отмечает клиент онлайн, обновляет его репо и паузу, отдаёт баланс и уведомления.
func (s *Service) Heartbeat(m Member, repos []string, pausedUntil int64, version string) (Beat, error) {
	if len(repos) > maxRepos {
		return Beat{}, userErr("слишком много проектов: до %d", maxRepos)
	}
	keys := make([]string, 0, len(repos))
	for _, r := range repos {
		repo, err := mrurl.ParseRepoKey(r)
		if err != nil {
			return Beat{}, userErr("проект %q: %v", r, err)
		}
		keys = append(keys, repo.String())
	}
	if len(version) > 40 {
		version = version[:40]
	}
	now := s.now()
	if pausedUntil < now {
		pausedUntil = 0
	}
	if max := now + int64(MaxPause/time.Second); pausedUntil > max {
		pausedUntil = max
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return Beat{}, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE members SET last_seen = ?, paused_until = ?, version = ? WHERE id = ?`, now, pausedUntil, version, m.ID); err != nil {
		return Beat{}, err
	}
	if _, err := tx.Exec(`DELETE FROM activations WHERE member_id = ?`, m.ID); err != nil {
		return Beat{}, err
	}
	for _, k := range keys {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO activations(member_id, repo) VALUES(?,?)`, m.ID, k); err != nil {
			return Beat{}, err
		}
	}
	if pausedUntil > now {
		// Задания, которые клиент ещё не забрал, отдаём другим: человек поставил паузу.
		if err := s.failJobs(tx, `j.member_id = ? AND j.status = 'assigned'`, []any{m.ID}, "участник поставил паузу"); err != nil {
			return Beat{}, err
		}
	}
	rows, err := tx.Query(`SELECT id, at, text FROM events WHERE member_id = ? AND delivered = 0 ORDER BY id LIMIT ?`, m.ID, maxEventsBatch)
	if err != nil {
		return Beat{}, err
	}
	var ids []int64
	events := []Event{}
	for rows.Next() {
		var id int64
		var e Event
		if err := rows.Scan(&id, &e.At, &e.Text); err != nil {
			rows.Close()
			return Beat{}, err
		}
		ids = append(ids, id)
		events = append(events, e)
	}
	rows.Close()
	for _, id := range ids {
		if _, err := tx.Exec(`UPDATE events SET delivered = 1 WHERE id = ?`, id); err != nil {
			return Beat{}, err
		}
	}
	if err := s.assignPending(tx); err != nil {
		return Beat{}, err
	}
	bal, err := balance(tx, m.ID)
	if err != nil {
		return Beat{}, err
	}
	if err := tx.Commit(); err != nil {
		return Beat{}, err
	}
	return Beat{Balance: bal, Events: events, ServerTime: now, Version: s.Version}, nil
}

type querier interface {
	QueryRow(query string, args ...any) *sql.Row
}

func balance(q querier, memberID int64) (int, error) {
	var b int
	err := q.QueryRow(`SELECT coalesce(sum(delta), 0) FROM ledger WHERE member_id = ?`, memberID).Scan(&b)
	return b, err
}

// --- запросы ревью ------------------------------------------------------------

// Requested — ответ на запрос ревью.
type Requested struct {
	ID        int64    `json:"id"`
	Reviewers []string `json:"reviewers"`
	Waiting   bool     `json:"waiting"`
	Balance   int      `json:"balance"`
}

// CreateRequest списывает балл и раздаёт задания ⌈50%⌉ подходящих участников.
func (s *Service) CreateRequest(m Member, mrURL string) (Requested, error) {
	mr, err := mrurl.ParseMR(mrURL)
	if err != nil {
		return Requested{}, userErr("ссылка на MR: %v", err)
	}
	repo := mr.Repo.String()
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return Requested{}, err
	}
	defer tx.Rollback()
	var lastSeen int64
	if err := tx.QueryRow(`SELECT last_seen FROM members WHERE id = ?`, m.ID).Scan(&lastSeen); err != nil {
		return Requested{}, err
	}
	if now-lastSeen > int64(OnlineWindow/time.Second) {
		return Requested{}, userErr("клиент cross-review не запущен: просить ревью можно, только когда он работает")
	}
	var active int
	if err := tx.QueryRow(`SELECT count(*) FROM activations WHERE member_id = ? AND repo = ?`, m.ID, repo).Scan(&active); err != nil {
		return Requested{}, err
	}
	if active == 0 {
		return Requested{}, userErr("проект %s не подключён в твоём клиенте", repo)
	}
	var open int
	if err := tx.QueryRow(`SELECT count(*) FROM requests WHERE mr_url = ? AND status = 'open'`, mr.String()).Scan(&open); err != nil {
		return Requested{}, err
	}
	if open > 0 {
		return Requested{}, userErr("по %s уже идёт ревью, дождись его конца", mr.Short())
	}
	bal, err := balance(tx, m.ID)
	if err != nil {
		return Requested{}, err
	}
	if bal < 1 {
		return Requested{}, userErr("баллов нет: сделай ревью чужого MR, чтобы попросить своё")
	}
	res, err := tx.Exec(`INSERT INTO requests(mr_url, repo, requester_id, created_at, status) VALUES(?,?,?,?, 'open')`, mr.String(), repo, m.ID, now)
	if err != nil {
		return Requested{}, err
	}
	reqID, _ := res.LastInsertId()
	if _, err := tx.Exec(`INSERT INTO ledger(member_id, delta, reason, request_id, at) VALUES(?, -1, 'request', ?, ?)`, m.ID, reqID, now); err != nil {
		return Requested{}, err
	}
	cands, err := s.eligible(tx, repo, map[int64]bool{m.ID: true})
	if err != nil {
		return Requested{}, err
	}
	out := Requested{ID: reqID, Reviewers: []string{}, Balance: bal - 1}
	if len(cands) == 0 {
		if _, err := tx.Exec(`INSERT INTO jobs(request_id, status, created_at) VALUES(?, 'pending', ?)`, reqID, now); err != nil {
			return Requested{}, err
		}
		out.Waiting = true
	}
	for _, c := range cands[:reviewerCount(len(cands))] {
		if err := assign(tx, reqID, c.ID, false, now); err != nil {
			return Requested{}, err
		}
		out.Reviewers = append(out.Reviewers, c.Name)
	}
	if err := tx.Commit(); err != nil {
		return Requested{}, err
	}
	return out, nil
}

// reviewerCount — ⌈50%⌉ подходящих: из 1 — один, из 4 — двое, из 5 — трое.
func reviewerCount(eligible int) int {
	if eligible == 0 {
		return 0
	}
	return (eligible + 1) / 2
}

type candidate struct {
	ID   int64
	Name string
}

// eligible — кто может взять ревью репо сейчас: онлайн, без паузы, не занят, проект подключён.
// Первыми идут те, кто дольше всех не получал заданий.
func (s *Service) eligible(tx *sql.Tx, repo string, exclude map[int64]bool) ([]candidate, error) {
	now := s.now()
	rows, err := tx.Query(`
		SELECT m.id, m.name FROM members m
		JOIN activations a ON a.member_id = m.id AND a.repo = ?
		WHERE m.revoked = 0 AND m.last_seen >= ? AND m.paused_until <= ?
		  AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.member_id = m.id AND j.status IN ('assigned', 'running'))
		ORDER BY m.last_assigned, m.id`,
		repo, now-int64(OnlineWindow/time.Second), now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.ID, &c.Name); err != nil {
			return nil, err
		}
		if !exclude[c.ID] {
			out = append(out, c)
		}
	}
	return out, rows.Err()
}

func assign(tx *sql.Tx, reqID, memberID int64, replacement bool, now int64) error {
	r := 0
	if replacement {
		r = 1
	}
	if _, err := tx.Exec(`INSERT INTO jobs(request_id, member_id, status, replacement, created_at, assigned_at) VALUES(?,?, 'assigned', ?,?,?)`, reqID, memberID, r, now, now); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE members SET last_assigned = ? WHERE id = ?`, now, memberID)
	return err
}

// assignPending раздаёт задания, которые ждут ревьюера: запрос без подходящих и замены упавших.
func (s *Service) assignPending(tx *sql.Tx) error {
	type pending struct {
		job, req, requester int64
		repo                string
	}
	rows, err := tx.Query(`
		SELECT j.id, r.id, r.requester_id, r.repo FROM jobs j JOIN requests r ON r.id = j.request_id
		WHERE j.status = 'pending' AND r.status = 'open' ORDER BY j.id`)
	if err != nil {
		return err
	}
	var list []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.job, &p.req, &p.requester, &p.repo); err != nil {
			rows.Close()
			return err
		}
		list = append(list, p)
	}
	rows.Close()
	now := s.now()
	for _, p := range list {
		exclude, err := requestMembers(tx, p.req)
		if err != nil {
			return err
		}
		exclude[p.requester] = true
		cands, err := s.eligible(tx, p.repo, exclude)
		if err != nil {
			return err
		}
		if len(cands) == 0 {
			continue
		}
		c := cands[0]
		if _, err := tx.Exec(`UPDATE jobs SET member_id = ?, status = 'assigned', assigned_at = ? WHERE id = ?`, c.ID, now, p.job); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE members SET last_assigned = ? WHERE id = ?`, now, c.ID); err != nil {
			return err
		}
		mr, err := requestMR(tx, p.req)
		if err != nil {
			return err
		}
		if err := addEvent(tx, p.requester, now, fmt.Sprintf("%s: ревью взял %s", mr.Short(), c.Name)); err != nil {
			return err
		}
	}
	return nil
}

func requestMembers(tx *sql.Tx, reqID int64) (map[int64]bool, error) {
	rows, err := tx.Query(`SELECT member_id FROM jobs WHERE request_id = ? AND member_id IS NOT NULL`, reqID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

func requestMR(q querier, reqID int64) (mrurl.MR, error) {
	var u string
	if err := q.QueryRow(`SELECT mr_url FROM requests WHERE id = ?`, reqID).Scan(&u); err != nil {
		return mrurl.MR{}, err
	}
	return mrurl.ParseMR(u)
}

func addEvent(tx *sql.Tx, memberID, at int64, text string) error {
	_, err := tx.Exec(`INSERT INTO events(member_id, at, text) VALUES(?,?,?)`, memberID, at, text)
	return err
}

// --- задания ------------------------------------------------------------------

// Job — задание ревьюеру: только ссылка на MR и кто просит.
type Job struct {
	ID        int64  `json:"job_id"`
	MRURL     string `json:"mr_url"`
	Requester string `json:"requester"`
}

// NextJob ждёт задание участнику до wait и отдаёт его, отмечая начатым.
func (s *Service) NextJob(ctx context.Context, m Member, wait time.Duration) (*Job, error) {
	deadline := time.Now().Add(wait)
	for {
		job, err := s.takeJob(m)
		if err != nil || job != nil {
			return job, err
		}
		if time.Now().After(deadline) {
			return nil, nil
		}
		select {
		case <-ctx.Done():
			return nil, nil
		case <-time.After(time.Second):
		}
	}
}

func (s *Service) takeJob(m Member) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := s.now()
	if _, err := tx.Exec(`UPDATE members SET last_seen = ? WHERE id = ?`, now, m.ID); err != nil {
		return nil, err
	}
	var j Job
	err = tx.QueryRow(`
		SELECT j.id, r.mr_url, mm.name FROM jobs j
		JOIN requests r ON r.id = j.request_id JOIN members mm ON mm.id = r.requester_id
		WHERE j.member_id = ? AND j.status = 'assigned' ORDER BY j.id LIMIT 1`, m.ID).Scan(&j.ID, &j.MRURL, &j.Requester)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, tx.Commit()
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE jobs SET status = 'running', started_at = ? WHERE id = ?`, now, j.ID); err != nil {
		return nil, err
	}
	return &j, tx.Commit()
}

// Result принимает итог задания: код 0 — ревью сделано и +1 балл, иначе задание уходит следующему.
func (s *Service) Result(m Member, jobID int64, exitCode int, errTail string) error {
	if len(errTail) > maxErrorTail {
		errTail = errTail[len(errTail)-maxErrorTail:]
	}
	errTail = strings.ToValidUTF8(errTail, "")
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	var reqID, requester int64
	err = tx.QueryRow(`SELECT j.status, j.request_id, r.requester_id FROM jobs j JOIN requests r ON r.id = j.request_id WHERE j.id = ? AND j.member_id = ?`, jobID, m.ID).Scan(&status, &reqID, &requester)
	if errors.Is(err, sql.ErrNoRows) {
		return userErr("задание %d не твоё", jobID)
	}
	if err != nil {
		return err
	}
	if status != "running" && status != "assigned" {
		return userErr("задание %d уже закрыто", jobID)
	}
	now := s.now()
	mr, err := requestMR(tx, reqID)
	if err != nil {
		return err
	}
	if exitCode == 0 {
		if _, err := tx.Exec(`UPDATE jobs SET status = 'done', finished_at = ?, exit_code = 0 WHERE id = ?`, now, jobID); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO ledger(member_id, delta, reason, request_id, job_id, at) VALUES(?, 1, 'review', ?, ?, ?)`, m.ID, reqID, jobID, now); err != nil {
			return err
		}
		if err := addEvent(tx, requester, now, fmt.Sprintf("%s: %s закончил ревью", mr.Short(), m.Name)); err != nil {
			return err
		}
	} else {
		if _, err := tx.Exec(`UPDATE jobs SET exit_code = ? WHERE id = ?`, exitCode, jobID); err != nil {
			return err
		}
		reason := fmt.Sprintf("команда ревью завершилась с кодом %d", exitCode)
		if err := s.failJobs(tx, `j.id = ?`, []any{jobID}, reason); err != nil {
			return err
		}
		if errTail != "" {
			if _, err := tx.Exec(`UPDATE jobs SET error = ? WHERE id = ?`, errTail, jobID); err != nil {
				return err
			}
		}
	}
	if err := s.assignPending(tx); err != nil {
		return err
	}
	if err := s.closeFinished(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// failJobs закрывает задания неудачей. Задание, которое ещё не заменяли, один раз уходит
// следующему подходящему.
func (s *Service) failJobs(tx *sql.Tx, where string, args []any, reason string) error {
	rows, err := tx.Query(`
		SELECT j.id, j.request_id, j.replacement, r.requester_id, coalesce(m.name, '') FROM jobs j
		JOIN requests r ON r.id = j.request_id LEFT JOIN members m ON m.id = j.member_id
		WHERE j.status IN ('assigned', 'running') AND `+where, args...)
	if err != nil {
		return err
	}
	type failed struct {
		id, req, requester int64
		replacement        bool
		name               string
	}
	var list []failed
	for rows.Next() {
		var f failed
		if err := rows.Scan(&f.id, &f.req, &f.replacement, &f.requester, &f.name); err != nil {
			rows.Close()
			return err
		}
		list = append(list, f)
	}
	rows.Close()
	now := s.now()
	for _, f := range list {
		if _, err := tx.Exec(`UPDATE jobs SET status = 'failed', finished_at = ?, error = ? WHERE id = ?`, now, reason, f.id); err != nil {
			return err
		}
		mr, err := requestMR(tx, f.req)
		if err != nil {
			return err
		}
		text := fmt.Sprintf("%s: у %s ревью не вышло (%s)", mr.Short(), f.name, reason)
		if !f.replacement {
			if _, err := tx.Exec(`INSERT INTO jobs(request_id, status, replacement, created_at) VALUES(?, 'pending', 1, ?)`, f.req, now); err != nil {
				return err
			}
			text += ", ищу замену"
		}
		if err := addEvent(tx, f.requester, now, text); err != nil {
			return err
		}
	}
	return nil
}

// closeFinished закрывает запросы, у которых не осталось незаконченных заданий.
func (s *Service) closeFinished(tx *sql.Tx) error {
	rows, err := tx.Query(`
		SELECT r.id, r.requester_id,
		       (SELECT count(*) FROM jobs WHERE request_id = r.id AND status = 'done'),
		       (SELECT count(*) FROM jobs WHERE request_id = r.id AND member_id IS NOT NULL)
		FROM requests r WHERE r.status = 'open'
		  AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.request_id = r.id AND j.status IN ('pending', 'assigned', 'running'))`)
	if err != nil {
		return err
	}
	type fin struct{ id, requester, done, total int64 }
	var list []fin
	for rows.Next() {
		var f fin
		if err := rows.Scan(&f.id, &f.requester, &f.done, &f.total); err != nil {
			rows.Close()
			return err
		}
		list = append(list, f)
	}
	rows.Close()
	now := s.now()
	for _, f := range list {
		if _, err := tx.Exec(`UPDATE requests SET status = 'done' WHERE id = ?`, f.id); err != nil {
			return err
		}
		mr, err := requestMR(tx, f.id)
		if err != nil {
			return err
		}
		if err := addEvent(tx, f.requester, now, fmt.Sprintf("%s: ревью закончены, %d из %d", mr.Short(), f.done, f.total)); err != nil {
			return err
		}
	}
	return nil
}

// Tick — фоновая уборка: пропавшие клиенты, зависшие задания, запросы без ревьюеров дольше суток.
func (s *Service) Tick() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now()
	if err := s.failJobs(tx, `j.member_id IN (SELECT id FROM members WHERE last_seen < ?)`, []any{now - int64(OfflineAfter/time.Second)}, "клиент пропал"); err != nil {
		return err
	}
	if err := s.failJobs(tx, `j.status = 'running' AND j.started_at < ?`, []any{now - int64(JobTimeout/time.Second)}, "дольше 60 минут"); err != nil {
		return err
	}
	if err := s.assignPending(tx); err != nil {
		return err
	}
	rows, err := tx.Query(`
		SELECT j.id, r.id, r.requester_id FROM jobs j JOIN requests r ON r.id = j.request_id
		WHERE j.status = 'pending' AND r.created_at < ?`, now-int64(RequestTTL/time.Second))
	if err != nil {
		return err
	}
	type exp struct{ job, req, requester int64 }
	var list []exp
	for rows.Next() {
		var e exp
		if err := rows.Scan(&e.job, &e.req, &e.requester); err != nil {
			rows.Close()
			return err
		}
		list = append(list, e)
	}
	rows.Close()
	for _, e := range list {
		if _, err := tx.Exec(`UPDATE jobs SET status = 'expired', finished_at = ? WHERE id = ?`, now, e.job); err != nil {
			return err
		}
	}
	if err := s.closeFinished(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// RunTicker вызывает Tick, пока жив ctx.
func (s *Service) RunTicker(ctx context.Context, every time.Duration, logf func(string, ...any)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Tick(); err != nil {
				logf("tick: %v", err)
			}
		}
	}
}

// RequestInfo — запрос участника со статусами заданий.
type RequestInfo struct {
	ID        int64     `json:"id"`
	MRURL     string    `json:"mr_url"`
	CreatedAt int64     `json:"created_at"`
	Status    string    `json:"status"`
	Jobs      []JobInfo `json:"jobs"`
}

// JobInfo — задание в запросе.
type JobInfo struct {
	Reviewer string `json:"reviewer"`
	Status   string `json:"status"`
}

// MyRequests — последние 20 запросов участника.
func (s *Service) MyRequests(m Member) ([]RequestInfo, error) {
	rows, err := s.db.Query(`SELECT id, mr_url, created_at, status FROM requests WHERE requester_id = ? ORDER BY id DESC LIMIT 20`, m.ID)
	if err != nil {
		return nil, err
	}
	out := []RequestInfo{}
	for rows.Next() {
		r := RequestInfo{Jobs: []JobInfo{}}
		if err := rows.Scan(&r.ID, &r.MRURL, &r.CreatedAt, &r.Status); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	rows.Close()
	for i := range out {
		jr, err := s.db.Query(`SELECT coalesce(m.name, ''), j.status FROM jobs j LEFT JOIN members m ON m.id = j.member_id WHERE j.request_id = ? ORDER BY j.id`, out[i].ID)
		if err != nil {
			return nil, err
		}
		for jr.Next() {
			var j JobInfo
			if err := jr.Scan(&j.Reviewer, &j.Status); err != nil {
				jr.Close()
				return nil, err
			}
			out[i].Jobs = append(out[i].Jobs, j)
		}
		jr.Close()
	}
	return out, nil
}

// Me — участник глазами брокера: для `cross-review status`, без отметки «онлайн».
type Me struct {
	Name        string `json:"name"`
	Balance     int    `json:"balance"`
	LastSeen    int64  `json:"last_seen"`
	PausedUntil int64  `json:"paused_until"`
	Repos       int    `json:"repos"`
}

// Whoami — баланс и активность участника.
func (s *Service) Whoami(m Member) (Me, error) {
	out := Me{Name: m.Name}
	err := s.db.QueryRow(`SELECT last_seen, paused_until, (SELECT count(*) FROM activations WHERE member_id = ?) FROM members WHERE id = ?`, m.ID, m.ID).Scan(&out.LastSeen, &out.PausedUntil, &out.Repos)
	if err != nil {
		return out, err
	}
	out.Balance, err = balance(s.db, m.ID)
	return out, err
}

// --- настройки и управление -----------------------------------------------------

func (s *Service) setting(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func newInvite() string {
	b := make([]byte, 15)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "inv_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}

// EnsureInvite создаёт приглашение, если его ещё нет. created — его создали сейчас.
func (s *Service) EnsureInvite() (invite string, created bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.setting("invite")
	if err != nil || cur != "" {
		return cur, false, err
	}
	inv := newInvite()
	_, err = s.db.Exec(`INSERT INTO settings(key, value) VALUES('invite', ?)`, inv)
	return inv, err == nil, err
}

// RotateInvite заменяет приглашение. Выданные ключи продолжают работать.
func (s *Service) RotateInvite() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inv := newInvite()
	_, err := s.db.Exec(`INSERT INTO settings(key, value) VALUES('invite', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, inv)
	return inv, err
}

// MemberInfo — строка списка участников для администратора.
type MemberInfo struct {
	Name        string
	Balance     int
	LastSeen    int64
	PausedUntil int64
	Repos       int
	Done        int
	Version     string
}

// Members — участники с балансом и активностью.
func (s *Service) Members() ([]MemberInfo, error) {
	rows, err := s.db.Query(`
		SELECT m.name, coalesce((SELECT sum(delta) FROM ledger WHERE member_id = m.id), 0),
		       m.last_seen, m.paused_until, (SELECT count(*) FROM activations WHERE member_id = m.id),
		       (SELECT count(*) FROM jobs WHERE member_id = m.id AND status = 'done'), m.version
		FROM members m WHERE m.revoked = 0 ORDER BY m.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemberInfo
	for rows.Next() {
		var mi MemberInfo
		if err := rows.Scan(&mi.Name, &mi.Balance, &mi.LastSeen, &mi.PausedUntil, &mi.Repos, &mi.Done, &mi.Version); err != nil {
			return nil, err
		}
		out = append(out, mi)
	}
	return out, rows.Err()
}

func (s *Service) memberID(tx *sql.Tx, name string) (int64, error) {
	var id int64
	err := tx.QueryRow(`SELECT id FROM members WHERE name_key = ? AND revoked = 0`, nameKey(name)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, userErr("участника %q нет", name)
	}
	return id, err
}

// Revoke отзывает ключ участника и освобождает имя. Его задания уходят другим.
func (s *Service) Revoke(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	id, err := s.memberID(tx, name)
	if err != nil {
		return err
	}
	if err := s.failJobs(tx, `j.member_id = ?`, []any{id}, "участник отключён"); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE members SET revoked = 1, key_hash = ?, name_key = name_key || ' #' || id WHERE id = ?`, "revoked:"+randomToken("", 16), id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM activations WHERE member_id = ?`, id); err != nil {
		return err
	}
	if err := s.assignPending(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Grant начисляет или списывает баллы вручную.
func (s *Service) Grant(name string, n int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	id, err := s.memberID(tx, name)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO ledger(member_id, delta, reason, at) VALUES(?,?, 'grant', ?)`, id, n, s.now()); err != nil {
		return err
	}
	return tx.Commit()
}
