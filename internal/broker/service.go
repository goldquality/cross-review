// Package broker — сервер cross-review: участники, баллы, запросы ревью и задания.
//
// Брокер раздаёт только ссылки на MR. Что запускать, решает клиент участника.
package broker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
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
	topCount       = 5
	// InviteTTL — сколько живёт неиспользованное личное приглашение.
	InviteTTL = 7 * 24 * time.Hour
	// MaxOpenRequests — сколько запросов участника могут идти одновременно.
	MaxOpenRequests = 5
	eventsKeep      = 30 * 24 * time.Hour
)

// Ошибки, которые видит участник.
var (
	ErrBadInvite = errors.New("приглашение не подходит")
	ErrBadKey    = errors.New("ключ не подходит или отозван")
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

	wakeMu sync.Mutex
	wake   chan struct{} // закрывается, когда задания могли поменяться: долгие опросы проверяют свои
	polls  map[int64]*poll
}

type poll struct{ cancel context.CancelFunc }

// NewService оборачивает открытую базу.
func NewService(db *sql.DB) *Service {
	return &Service{db: db, Now: time.Now, wake: make(chan struct{}), polls: map[int64]*poll{}}
}

// commit фиксирует транзакцию и будит долгие опросы: она могла выдать задания.
func (s *Service) commit(tx *sql.Tx) error {
	if err := tx.Commit(); err != nil {
		return err
	}
	s.wakeMu.Lock()
	close(s.wake)
	s.wake = make(chan struct{})
	s.wakeMu.Unlock()
	return nil
}

func (s *Service) wakeCh() <-chan struct{} {
	s.wakeMu.Lock()
	defer s.wakeMu.Unlock()
	return s.wake
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

// confusable — кириллица, неотличимая от латиницы. Имена сравниваются по «скелету», чтобы «Аlice»
// с русской А не выдала себя за Alice.
var confusable = strings.NewReplacer("а", "a", "в", "b", "е", "e", "ё", "e", "к", "k", "м", "m", "н", "h",
	"о", "o", "р", "p", "с", "c", "т", "t", "у", "y", "х", "x", "і", "i", "ј", "j", "ѕ", "s")

func skeleton(name string) string { return confusable.Replace(nameKey(name)) }

// checkName — имя участника: буквы одного алфавита, цифры, пробел, точка, дефис, до 40 символов.
func checkName(name string) error {
	if utf8.RuneCountInString(name) > 40 || !nameRe.MatchString(name) {
		return userErr("имя: буквы, цифры, пробел, точка, дефис, до 40 символов")
	}
	var script *unicode.RangeTable
	for _, r := range name {
		if !unicode.IsLetter(r) {
			continue
		}
		cur := scriptOf(r)
		if script != nil && cur != script {
			return userErr("имя: буквы одного алфавита, без смеси латиницы и кириллицы")
		}
		script = cur
	}
	return nil
}

func scriptOf(r rune) *unicode.RangeTable {
	for _, t := range []*unicode.RangeTable{unicode.Latin, unicode.Cyrillic, unicode.Greek} {
		if unicode.Is(t, r) {
			return t
		}
	}
	return otherScript // прочие алфавиты считаем одним: в команде их нет
}

var otherScript = &unicode.RangeTable{}

// Invite — личное приглашение: создаёт администратор, используется один раз.
type Invite struct {
	Code      string // показывается один раз, в базе только хэш
	Name      string
	ExpiresAt int64
	Rekey     bool // участник с этим именем уже есть: вход заменит его ключ, баллы останутся
}

// CreateInvite выдаёт личное приглашение на имя. Прежнее приглашение на это имя перестаёт
// работать. Если участник с таким именем уже есть, вход по приглашению выдаст ему новый ключ: так
// возвращают того, кто потерял состояние клиента или был отозван.
func (s *Service) CreateInvite(name string) (Invite, error) {
	name = strings.TrimSpace(name)
	if err := checkName(name); err != nil {
		return Invite{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return Invite{}, err
	}
	defer tx.Rollback()
	key, skel := nameKey(name), skeleton(name)
	rows, err := tx.Query(`SELECT name, name_key FROM members UNION ALL SELECT name, name_key FROM invites`)
	if err != nil {
		return Invite{}, err
	}
	inv := Invite{Name: name}
	for rows.Next() {
		var other, otherKey string
		if err := rows.Scan(&other, &otherKey); err != nil {
			rows.Close()
			return Invite{}, err
		}
		if otherKey != key && skeleton(other) == skel {
			rows.Close()
			return Invite{}, userErr("имя %q не отличить на глаз от %q", name, other)
		}
	}
	rows.Close()
	var existing int
	if err := tx.QueryRow(`SELECT count(*) FROM members WHERE name_key = ?`, key).Scan(&existing); err != nil {
		return Invite{}, err
	}
	inv.Rekey = existing > 0
	inv.Code = newInvite()
	now := s.now()
	inv.ExpiresAt = now + int64(InviteTTL/time.Second)
	if _, err := tx.Exec(`DELETE FROM invites WHERE name_key = ? OR expires_at < ?`, key, now); err != nil {
		return Invite{}, err
	}
	if _, err := tx.Exec(`INSERT INTO invites(code_hash, name, name_key, created_at, expires_at) VALUES(?,?,?,?,?)`, hashKey(inv.Code), name, key, now, inv.ExpiresAt); err != nil {
		return Invite{}, err
	}
	return inv, tx.Commit()
}

// PendingInvite — неиспользованное приглашение для списка администратора, без кода.
type PendingInvite struct {
	Name      string
	ExpiresAt int64
}

// Invites — неиспользованные приглашения, которые ещё действуют.
func (s *Service) Invites() ([]PendingInvite, error) {
	rows, err := s.db.Query(`SELECT name, expires_at FROM invites WHERE expires_at >= ? ORDER BY name_key`, s.now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingInvite
	for rows.Next() {
		var p PendingInvite
		if err := rows.Scan(&p.Name, &p.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Join меняет личное приглашение на ключ. Имя задаёт приглашение, а не клиент. Новый участник
// получает стартовые баллы; участник, которому выдали приглашение повторно, — новый ключ и прежний
// баланс.
func (s *Service) Join(invite string) (Joined, error) {
	code := strings.TrimSpace(invite)
	if !strings.HasPrefix(code, "inv_") {
		return Joined{}, ErrBadInvite
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return Joined{}, err
	}
	defer tx.Rollback()
	now := s.now()
	var name, nkey string
	err = tx.QueryRow(`SELECT name, name_key FROM invites WHERE code_hash = ? AND expires_at >= ?`, hashKey(code), now).Scan(&name, &nkey)
	if errors.Is(err, sql.ErrNoRows) {
		return Joined{}, ErrBadInvite
	}
	if err != nil {
		return Joined{}, err
	}
	if _, err := tx.Exec(`DELETE FROM invites WHERE code_hash = ?`, hashKey(code)); err != nil {
		return Joined{}, err
	}
	key := randomToken("crk_", 32)
	var id int64
	err = tx.QueryRow(`SELECT id FROM members WHERE name_key = ?`, nkey).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		res, err := tx.Exec(`INSERT INTO members(name, name_key, key_hash, created_at) VALUES(?,?,?,?)`, name, nkey, hashKey(key), now)
		if err != nil {
			return Joined{}, err
		}
		id, _ = res.LastInsertId()
		if _, err := tx.Exec(`INSERT INTO ledger(member_id, delta, reason, at) VALUES(?,?,?,?)`, id, StartBalance, "start", now); err != nil {
			return Joined{}, err
		}
	case err != nil:
		return Joined{}, err
	default:
		if _, err := tx.Exec(`UPDATE members SET key_hash = ?, revoked = 0, name = ? WHERE id = ?`, hashKey(key), name, id); err != nil {
			return Joined{}, err
		}
	}
	bal, err := balance(tx, id)
	if err != nil {
		return Joined{}, err
	}
	if err := tx.Commit(); err != nil {
		return Joined{}, err
	}
	return Joined{Key: key, Name: name, Balance: bal}, nil
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
	Team       Team    `json:"team"`
	// Ignored — репо клиента, которых нет в списке разрешённых: по ним задания не идут.
	Ignored []string `json:"ignored"`
}

// Team — команда для меню трея: кто онлайн и у кого больше всех баллов.
type Team struct {
	Online []Teammate `json:"online"` // по имени
	Top    []Teammate `json:"top"`    // topCount участников с наибольшим балансом
}

// Teammate — участник в меню трея. State есть только у тех, кто онлайн: free, offered (думает над
// предложенным запросом), busy (ревьюит), paused.
type Teammate struct {
	Name    string `json:"name"`
	Balance int    `json:"balance"`
	State   string `json:"state,omitempty"`
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
	if !versionRe.MatchString(version) {
		version = "?" // строка уходит в терминал администратора: без управляющих символов
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
	ignored := []string{}
	for _, k := range keys {
		res, err := tx.Exec(`INSERT OR IGNORE INTO activations(member_id, repo) SELECT ?, repo FROM repos WHERE repo = ?`, m.ID, k)
		if err != nil {
			return Beat{}, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			ignored = append(ignored, k)
		}
	}
	var withdrawn []int64
	if pausedUntil > now {
		// Предложения, которые клиент ещё не забрал, снимаем: человек поставил паузу.
		if withdrawn, err = s.withdrawOffers(tx, `member_id = ?`, []any{m.ID}); err != nil {
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
	if err := s.afterJobsClosed(tx, withdrawn...); err != nil {
		return Beat{}, err
	}
	bal, err := balance(tx, m.ID)
	if err != nil {
		return Beat{}, err
	}
	team, err := s.team(tx)
	if err != nil {
		return Beat{}, err
	}
	if err := s.commit(tx); err != nil {
		return Beat{}, err
	}
	return Beat{Balance: bal, Events: events, ServerTime: now, Version: s.Version, Team: team, Ignored: ignored}, nil
}

var versionRe = regexp.MustCompile(`^v?[0-9A-Za-z.+-]{0,40}$`)

// legacyClient — клиент до v0.1.4: он не спрашивает «принять?» и не сообщает о согласии, поэтому
// взятое им задание — уже ревью, а не раздумье.
func legacyClient(version string) bool {
	var a, b, c int
	if n, _ := fmt.Sscanf(version, "v%d.%d.%d", &a, &b, &c); n != 3 {
		return false
	}
	return a == 0 && (b == 0 || b == 1 && c < 4)
}

// team — кто онлайн и топ по балансу. Команда — десяток человек, поэтому читаем всех разом.
func (s *Service) team(tx *sql.Tx) (Team, error) {
	now := s.now()
	rows, err := tx.Query(`
		SELECT m.name, coalesce((SELECT sum(delta) FROM ledger WHERE member_id = m.id), 0), m.last_seen, m.paused_until, m.version,
		       EXISTS (SELECT 1 FROM jobs j WHERE j.member_id = m.id AND j.status = 'running' AND j.accepted_at > 0),
		       EXISTS (SELECT 1 FROM jobs j WHERE j.member_id = m.id AND j.status IN ('assigned', 'running'))
		FROM members m WHERE m.revoked = 0`)
	if err != nil {
		return Team{}, err
	}
	defer rows.Close()
	t := Team{Online: []Teammate{}, Top: []Teammate{}}
	for rows.Next() {
		var tm Teammate
		var lastSeen, paused int64
		var version string
		var accepted, busy bool
		if err := rows.Scan(&tm.Name, &tm.Balance, &lastSeen, &paused, &version, &accepted, &busy); err != nil {
			return Team{}, err
		}
		t.Top = append(t.Top, tm)
		if now-lastSeen > int64(OnlineWindow/time.Second) {
			continue
		}
		switch {
		case accepted || busy && legacyClient(version):
			tm.State = "busy"
		case busy:
			tm.State = "offered"
		case paused > now:
			tm.State = "paused"
		default:
			tm.State = "free"
		}
		t.Online = append(t.Online, tm)
	}
	if err := rows.Err(); err != nil {
		return Team{}, err
	}
	sort.Slice(t.Online, func(i, j int) bool { return nameKey(t.Online[i].Name) < nameKey(t.Online[j].Name) })
	sort.SliceStable(t.Top, func(i, j int) bool {
		if t.Top[i].Balance != t.Top[j].Balance {
			return t.Top[i].Balance > t.Top[j].Balance
		}
		return nameKey(t.Top[i].Name) < nameKey(t.Top[j].Name)
	})
	if len(t.Top) > topCount {
		t.Top = t.Top[:topCount]
	}
	return t, nil
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

// CreateRequest списывает балл и предлагает ревью всем свободным участникам с этим проектом.
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
		return Requested{}, userErr("проект %s не подключён в твоём клиенте или не разрешён на брокере", repo)
	}
	var mine int
	if err := tx.QueryRow(`SELECT count(*) FROM requests WHERE requester_id = ? AND status = 'open'`, m.ID).Scan(&mine); err != nil {
		return Requested{}, err
	}
	if mine >= MaxOpenRequests {
		return Requested{}, userErr("у тебя уже %d запросов в работе: дождись, пока какой-то закончится", mine)
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
	// Запрос предлагается всем свободным, ревьюит каждый, кто примет. Свободных нет — запрос ждёт:
	// его получат те, кто освободится или придёт, пока он открыт (offerOpen).
	out := Requested{ID: reqID, Reviewers: []string{}, Balance: bal - 1, Waiting: len(cands) == 0}
	for _, c := range cands {
		if err := assign(tx, reqID, c.ID, now); err != nil {
			return Requested{}, err
		}
		out.Reviewers = append(out.Reviewers, c.Name)
	}
	if err := s.commit(tx); err != nil {
		return Requested{}, err
	}
	return out, nil
}

type candidate struct {
	ID   int64
	Name string
}

// eligible — кто может взять ревью репо сейчас: онлайн, без паузы, не занят, проект подключён.
func (s *Service) eligible(tx *sql.Tx, repo string, exclude map[int64]bool) ([]candidate, error) {
	now := s.now()
	rows, err := tx.Query(`
		SELECT m.id, m.name FROM members m
		JOIN activations a ON a.member_id = m.id AND a.repo = ?
		WHERE `+freeMember+`
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

// freeMember — участник m может взять задание: не отозван, онлайн, без паузы, не занят. Параметры:
// граница «онлайн» и сейчас.
const freeMember = `m.revoked = 0 AND m.last_seen >= ? AND m.paused_until <= ?
		  AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.member_id = m.id AND j.status IN ('assigned', 'running'))`

func assign(tx *sql.Tx, reqID, memberID int64, now int64) error {
	if _, err := tx.Exec(`INSERT INTO jobs(request_id, member_id, status, created_at, assigned_at) VALUES(?,?, 'assigned', ?,?)`, reqID, memberID, now, now); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE members SET last_assigned = ? WHERE id = ?`, now, memberID)
	return err
}

// offerOpen предлагает открытые запросы свободным участникам, которым их ещё не предлагали: тем, кто
// пришёл, освободился или снял паузу, пока запрос открыт. У участника одно предложение за раз,
// старые запросы — первыми. Просящему не пишем: «взял ревью» он узнает, когда ревьюер примет.
func (s *Service) offerOpen(tx *sql.Tx) error {
	now := s.now()
	// Только запросы, у которых сейчас есть кому предложить: остальные перебирать незачем.
	rows, err := tx.Query(`
		SELECT r.id, r.requester_id, r.repo FROM requests r
		WHERE r.status = 'open' AND r.created_at >= ?
		  AND EXISTS (SELECT 1 FROM activations a JOIN members m ON m.id = a.member_id
		              WHERE a.repo = r.repo AND m.id != r.requester_id AND `+freeMember+`
		                AND NOT EXISTS (SELECT 1 FROM jobs o WHERE o.request_id = r.id AND o.member_id = m.id))
		ORDER BY r.id`, now-int64(RequestTTL/time.Second), now-int64(OnlineWindow/time.Second), now)
	if err != nil {
		return err
	}
	type open struct {
		req, requester int64
		repo           string
	}
	var list []open
	for rows.Next() {
		var o open
		if err := rows.Scan(&o.req, &o.requester, &o.repo); err != nil {
			rows.Close()
			return err
		}
		list = append(list, o)
	}
	rows.Close()
	for _, o := range list {
		exclude, err := requestMembers(tx, o.req)
		if err != nil {
			return err
		}
		exclude[o.requester] = true
		cands, err := s.eligible(tx, o.repo, exclude) // занятые предыдущим запросом сюда уже не попадут
		if err != nil {
			return err
		}
		for _, c := range cands {
			if err := assign(tx, o.req, c.ID, now); err != nil {
				return err
			}
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

// NextJob ждёт задание участнику до wait и отдаёт его, отмечая начатым. У участника один долгий
// опрос: новый отменяет прежний, чтобы один ключ не занял брокер сотней соединений. Ожидание не
// трогает базу: опрос просыпается, когда транзакция могла выдать задание.
func (s *Service) NextJob(ctx context.Context, m Member, wait time.Duration) (*Job, error) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	me := &poll{cancel: cancel}
	s.wakeMu.Lock()
	if prev := s.polls[m.ID]; prev != nil {
		prev.cancel()
	}
	s.polls[m.ID] = me
	s.wakeMu.Unlock()
	defer func() {
		s.wakeMu.Lock()
		if s.polls[m.ID] == me {
			delete(s.polls, m.ID)
		}
		s.wakeMu.Unlock()
	}()
	for {
		woken := s.wakeCh() // до проверки: задание, выданное между проверкой и ожиданием, не потеряется
		job, err := s.takeJob(m)
		if err != nil || job != nil {
			return job, err
		}
		select {
		case <-ctx.Done():
			return nil, nil
		case <-woken:
		}
	}
}

func (s *Service) takeJob(m Member) (*Job, error) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM jobs WHERE member_id = ? AND status = 'assigned' ORDER BY id LIMIT 1`, m.ID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE jobs SET status = 'running', started_at = ? WHERE id = ? AND status = 'assigned'`, s.now(), id)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, nil // задание успели отдать другому
	}
	j := Job{ID: id}
	if err := tx.QueryRow(`
		SELECT r.mr_url, mm.name FROM jobs j
		JOIN requests r ON r.id = j.request_id JOIN members mm ON mm.id = r.requester_id
		WHERE j.id = ?`, id).Scan(&j.MRURL, &j.Requester); err != nil {
		return nil, err
	}
	return &j, tx.Commit()
}

// ownJob — задание участника, которое ещё не закрыто: запрос и просящий.
func ownJob(tx *sql.Tx, m Member, jobID int64) (reqID, requester int64, err error) {
	var status string
	err = tx.QueryRow(`SELECT j.status, j.request_id, r.requester_id FROM jobs j JOIN requests r ON r.id = j.request_id WHERE j.id = ? AND j.member_id = ?`, jobID, m.ID).Scan(&status, &reqID, &requester)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, userErr("задание %d не твоё", jobID)
	}
	if err != nil {
		return 0, 0, err
	}
	if status != "running" && status != "assigned" {
		return 0, 0, userErr("задание %d уже закрыто", jobID)
	}
	return reqID, requester, nil
}

// Accept — участник согласился на ревью. 60 минут на ревью отсчитываются с этого момента, а не с
// выдачи: пока человек думал, время не шло.
func (s *Service) Accept(m Member, jobID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	reqID, requester, err := ownJob(tx, m, jobID)
	if err != nil {
		return err
	}
	now := s.now()
	if _, err := tx.Exec(`UPDATE jobs SET status = 'running', started_at = ?, accepted_at = ? WHERE id = ?`, now, now, jobID); err != nil {
		return err
	}
	mr, err := requestMR(tx, reqID)
	if err != nil {
		return err
	}
	if err := addEvent(tx, requester, now, fmt.Sprintf("%s: %s взял ревью", mr.Short(), m.Name)); err != nil {
		return err
	}
	return tx.Commit()
}

// Decline — участник отказался. Запрос и так предложен всем свободным, поэтому отказ просто закрывает
// предложение. Просящему пишем, только если запрос остался без ревьюеров.
func (s *Service) Decline(m Member, jobID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	reqID, _, err := ownJob(tx, m, jobID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE jobs SET status = 'declined', finished_at = ? WHERE id = ?`, s.now(), jobID); err != nil {
		return err
	}
	if err := s.afterJobsClosed(tx, reqID); err != nil {
		return err
	}
	return s.commit(tx)
}

// afterJobsClosed — задания закрылись: освободившимся участникам — открытые запросы, затронутым
// запросам без заданий — сказать просящему, что запрос ждёт, а готовые — закрыть.
func (s *Service) afterJobsClosed(tx *sql.Tx, reqIDs ...int64) error {
	if err := s.offerOpen(tx); err != nil {
		return err
	}
	seen := map[int64]bool{}
	for _, id := range reqIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		if err := s.noteStalled(tx, id); err != nil {
			return err
		}
	}
	return s.closeFinished(tx)
}

// noteStalled пишет просящему, если у открытого запроса не осталось ни предложений, ни готовых ревью:
// все отказались или упали, и запрос ждёт тех, кто освободится.
func (s *Service) noteStalled(tx *sql.Tx, reqID int64) error {
	var requester, created int64
	var active, done int
	err := tx.QueryRow(`
		SELECT r.requester_id, r.created_at,
		       (SELECT count(*) FROM jobs WHERE request_id = r.id AND status IN ('assigned', 'running')),
		       (SELECT count(*) FROM jobs WHERE request_id = r.id AND status = 'done')
		FROM requests r WHERE r.id = ? AND r.status = 'open'`, reqID).Scan(&requester, &created, &active, &done)
	// Запрос старше суток закроет closeFinished — писать «ждёт» уже нечего.
	if errors.Is(err, sql.ErrNoRows) || err == nil && (active > 0 || done > 0 || s.now()-created > int64(RequestTTL/time.Second)) {
		return nil
	}
	if err != nil {
		return err
	}
	mr, err := requestMR(tx, reqID)
	if err != nil {
		return err
	}
	until := time.Unix(created, 0).Add(RequestTTL).In(time.Local).Format("02.01 15:04")
	return addEvent(tx, requester, s.now(), fmt.Sprintf("%s: сейчас ревью никто не взял — запрос ждёт до %s тех, кто освободится", mr.Short(), until))
}

// Result принимает итог задания: код 0 — ревью сделано и +1 балл, иначе неудача.
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
	reqID, requester, err := ownJob(tx, m, jobID)
	if err != nil {
		return err
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
		if _, err := s.failJobs(tx, `j.id = ?`, []any{jobID}, reason); err != nil {
			return err
		}
		if errTail != "" {
			if _, err := tx.Exec(`UPDATE jobs SET error = ? WHERE id = ?`, errTail, jobID); err != nil {
				return err
			}
		}
	}
	if err := s.afterJobsClosed(tx, reqID); err != nil {
		return err
	}
	return s.commit(tx)
}

// failJobs закрывает неудачей начатые задания и пишет просящему; возвращает их запросы. Запрос
// остаётся открытым: если других ревью нет, его получат те, кто освободится.
func (s *Service) failJobs(tx *sql.Tx, where string, args []any, reason string) ([]int64, error) {
	rows, err := tx.Query(`
		SELECT j.id, j.request_id, r.requester_id, coalesce(m.name, '') FROM jobs j
		JOIN requests r ON r.id = j.request_id LEFT JOIN members m ON m.id = j.member_id
		WHERE j.status = 'running' AND `+where, args...)
	if err != nil {
		return nil, err
	}
	type failed struct {
		id, req, requester int64
		name               string
	}
	var list []failed
	for rows.Next() {
		var f failed
		if err := rows.Scan(&f.id, &f.req, &f.requester, &f.name); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, f)
	}
	rows.Close()
	now := s.now()
	var reqs []int64
	for _, f := range list {
		if _, err := tx.Exec(`UPDATE jobs SET status = 'failed', finished_at = ?, error = ? WHERE id = ?`, now, reason, f.id); err != nil {
			return nil, err
		}
		mr, err := requestMR(tx, f.req)
		if err != nil {
			return nil, err
		}
		if err := addEvent(tx, f.requester, now, fmt.Sprintf("%s: у %s ревью не вышло (%s)", mr.Short(), f.name, reason)); err != nil {
			return nil, err
		}
		reqs = append(reqs, f.req)
	}
	return reqs, nil
}

// withdrawOffers снимает предложения, которые клиент ещё не забрал: участник на паузе, пропал или
// отозван. Просящему не пишем — ревью никто и не начинал.
func (s *Service) withdrawOffers(tx *sql.Tx, where string, args []any) ([]int64, error) {
	rows, err := tx.Query(`SELECT id, request_id FROM jobs WHERE status = 'assigned' AND `+where, args...)
	if err != nil {
		return nil, err
	}
	var jobs, reqs []int64
	for rows.Next() {
		var j, r int64
		if err := rows.Scan(&j, &r); err != nil {
			rows.Close()
			return nil, err
		}
		jobs, reqs = append(jobs, j), append(reqs, r)
	}
	rows.Close()
	for _, j := range jobs {
		if _, err := tx.Exec(`UPDATE jobs SET status = 'withdrawn', finished_at = ? WHERE id = ?`, s.now(), j); err != nil {
			return nil, err
		}
	}
	return reqs, nil
}

// closeFinished закрывает запросы без незаконченных заданий, если хоть одно ревью готово или прошли
// сутки. Без готовых ревью запрос до суток ждёт тех, кто освободится.
func (s *Service) closeFinished(tx *sql.Tx) error {
	rows, err := tx.Query(`
		SELECT r.id, r.requester_id,
		       (SELECT count(*) FROM jobs WHERE request_id = r.id AND status = 'done'),
		       (SELECT count(*) FROM jobs WHERE request_id = r.id AND status IN ('done', 'failed'))
		FROM requests r WHERE r.status = 'open'
		  AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.request_id = r.id AND j.status IN ('assigned', 'running'))
		  AND (EXISTS (SELECT 1 FROM jobs j WHERE j.request_id = r.id AND j.status = 'done') OR r.created_at < ?)`,
		s.now()-int64(RequestTTL/time.Second))
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
		text := fmt.Sprintf("%s: ревью закончены, %d из %d", mr.Short(), f.done, f.total)
		if f.total == 0 {
			text = fmt.Sprintf("%s: за сутки ревью никто не взял, запрос закрыт", mr.Short())
		}
		if err := addEvent(tx, f.requester, now, text); err != nil {
			return err
		}
	}
	return nil
}

// Tick — фоновая уборка: пропавшие клиенты, зависшие задания, запросы старше суток.
func (s *Service) Tick() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now()
	offline := now - int64(OfflineAfter/time.Second)
	withdrawn, err := s.withdrawOffers(tx, `member_id IN (SELECT id FROM members WHERE last_seen < ?)`, []any{offline})
	if err != nil {
		return err
	}
	gone, err := s.failJobs(tx, `j.member_id IN (SELECT id FROM members WHERE last_seen < ?)`, []any{offline}, "клиент пропал")
	if err != nil {
		return err
	}
	stuck, err := s.failJobs(tx, `j.started_at < ?`, []any{now - int64(JobTimeout/time.Second)}, "дольше 60 минут")
	if err != nil {
		return err
	}
	if err := s.afterJobsClosed(tx, append(append(withdrawn, gone...), stuck...)...); err != nil {
		return err
	}
	// Сутки прошли: предложения, которые клиент не забрал, снимаются; начатые ревью доживают. 'pending'
	// — ждущие задания брокера до v0.1.5.
	if _, err := tx.Exec(`UPDATE jobs SET status = CASE status WHEN 'pending' THEN 'expired' ELSE 'withdrawn' END, finished_at = ?
		WHERE status IN ('assigned', 'pending') AND request_id IN (SELECT id FROM requests WHERE status = 'open' AND created_at < ?)`,
		now, now-int64(RequestTTL/time.Second)); err != nil {
		return err
	}
	if err := s.closeFinished(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM events WHERE delivered = 1 AND at < ?`, now-int64(eventsKeep/time.Second)); err != nil {
		return err
	}
	return s.commit(tx)
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
		jr, err := s.db.Query(`SELECT coalesce(m.name, ''), j.status, j.accepted_at, coalesce(m.version, '') FROM jobs j LEFT JOIN members m ON m.id = j.member_id WHERE j.request_id = ? ORDER BY j.id`, out[i].ID)
		if err != nil {
			return nil, err
		}
		waiting := true
		for jr.Next() {
			var j JobInfo
			var accepted int64
			var version string
			if err := jr.Scan(&j.Reviewer, &j.Status, &accepted, &version); err != nil {
				jr.Close()
				return nil, err
			}
			switch {
			case j.Status == "withdrawn":
				continue // предложение никто не открывал: участник ушёл на паузу или пропал
			case j.Status == "assigned", j.Status == "running" && accepted == 0 && !legacyClient(version):
				j.Status = "offered"
			}
			if j.Status == "offered" || j.Status == "running" || j.Status == "pending" {
				waiting = false
			}
			out[i].Jobs = append(out[i].Jobs, j)
		}
		jr.Close()
		if out[i].Status == "open" && waiting {
			out[i].Jobs = append(out[i].Jobs, JobInfo{Status: "pending"}) // ждёт тех, кто освободится
		}
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

// Repos — разрешённые репо: только по ним участники ревьюят и просят ревью.
func (s *Service) Repos() ([]string, error) {
	rows, err := s.db.Query(`SELECT repo FROM repos ORDER BY repo`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// parseRepoArg — репо от администратора: адрес как в git remote или браузере либо ключ
// host/group/project, как его показывает `broker repos`.
func parseRepoArg(addr string) (mrurl.Repo, error) {
	addr = strings.TrimSpace(addr)
	if !strings.Contains(addr, ":") {
		addr = "https://" + addr
	}
	repo, err := mrurl.ParseRemote(addr)
	if err != nil {
		return mrurl.Repo{}, userErr("репо %q: %v", addr, err)
	}
	return repo, nil
}

// AddRepo разрешает репо.
func (s *Service) AddRepo(addr string) (string, error) {
	repo, err := parseRepoArg(addr)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.db.Exec(`INSERT OR IGNORE INTO repos(repo) VALUES(?)`, repo.String())
	return repo.String(), err
}

// RemoveRepo запрещает репо. Участники перестают получать по нему задания сразу, а не со
// следующей отметкой.
func (s *Service) RemoveRepo(addr string) (string, error) {
	repo, err := parseRepoArg(addr)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`DELETE FROM repos WHERE repo = ?`, repo.String())
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", userErr("репо %s нет в списке", repo)
	}
	if _, err := tx.Exec(`DELETE FROM activations WHERE repo = ?`, repo.String()); err != nil {
		return "", err
	}
	return repo.String(), tx.Commit()
}

// Snapshot пишет целостную копию базы в path: её и забирает бэкап. Копировать живые файлы базы
// нельзя — основной файл и журнал WAL попадут в бэкап из разных моментов.
func (s *Service) Snapshot(path string) error {
	tmp := fmt.Sprintf("%s.%s.tmp", path, randomToken("", 6))
	if _, err := s.db.Exec(`VACUUM INTO ?`, tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
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

// Revoke отзывает ключ участника. Его задания уходят другим, имя остаётся занятым.
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
	withdrawn, err := s.withdrawOffers(tx, `member_id = ?`, []any{id})
	if err != nil {
		return err
	}
	failed, err := s.failJobs(tx, `j.member_id = ?`, []any{id}, "участник отключён")
	if err != nil {
		return err
	}
	// Имя остаётся за участником: занять его можно только приглашением от администратора.
	if _, err := tx.Exec(`UPDATE members SET revoked = 1, key_hash = ? WHERE id = ?`, "revoked:"+randomToken("", 16), id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM activations WHERE member_id = ?`, id); err != nil {
		return err
	}
	if err := s.afterJobsClosed(tx, append(withdrawn, failed...)...); err != nil {
		return err
	}
	return s.commit(tx)
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
