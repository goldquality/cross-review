package broker

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	repoKey = "gitlab.example.com/acme/shop"
	mr9     = "https://gitlab.example.com/acme/shop/-/merge_requests/9"
	mr10    = "https://gitlab.example.com/acme/shop/-/merge_requests/10"
)

type env struct {
	t   *testing.T
	s   *Service
	now time.Time
	inv string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e := &env{t: t, s: NewService(db), now: time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)}
	e.s.Now = func() time.Time { return e.now }
	inv, created, err := e.s.EnsureInvite()
	if err != nil || !created {
		t.Fatal(inv, created, err)
	}
	e.inv = inv
	return e
}

func (e *env) join(name string) Member {
	e.t.Helper()
	j, err := e.s.Join(e.inv, name)
	if err != nil {
		e.t.Fatal(err)
	}
	m, err := e.s.Auth(j.Key)
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

func (e *env) beat(m Member, repos ...string) Beat {
	e.t.Helper()
	b, err := e.s.Heartbeat(m, repos, 0, "test")
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

func (e *env) balance(m Member) int {
	e.t.Helper()
	b, err := balance(e.s.db, m.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

func (e *env) take(m Member) *Job {
	e.t.Helper()
	j, err := e.s.takeJob(m)
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

func (e *env) advance(d time.Duration) { e.now = e.now.Add(d) }

func isUserErr(err error) bool {
	var ue *UserError
	return errors.As(err, &ue)
}

func TestJoin(t *testing.T) {
	e := newEnv(t)
	if _, err := e.s.Join("inv_wrong", "Петя"); !errors.Is(err, ErrBadInvite) {
		t.Fatal(err)
	}
	j, err := e.s.Join(e.inv, "Петя")
	if err != nil || j.Balance != StartBalance || !strings.HasPrefix(j.Key, "crk_") {
		t.Fatal(j, err)
	}
	if _, err := e.s.Join(e.inv, "петя"); !errors.Is(err, ErrNameTaken) {
		t.Fatal("имя без учёта регистра должно быть занято:", err)
	}
	for _, bad := range []string{"", " ", "a;b", "<script>", strings.Repeat("я", 41)} {
		if _, err := e.s.Join(e.inv, bad); !isUserErr(err) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	if _, err := e.s.Auth("crk_nope"); !errors.Is(err, ErrBadKey) {
		t.Fatal(err)
	}
	m, err := e.s.Auth(j.Key)
	if err != nil || m.Name != "Петя" || e.balance(m) != 2 {
		t.Fatal(m, err)
	}
}

func TestRotateInviteKeepsKeys(t *testing.T) {
	e := newEnv(t)
	j, _ := e.s.Join(e.inv, "Петя")
	inv2, err := e.s.RotateInvite()
	if err != nil || inv2 == e.inv {
		t.Fatal(inv2, err)
	}
	if _, err := e.s.Join(e.inv, "Маша"); !errors.Is(err, ErrBadInvite) {
		t.Fatal("старое приглашение должно перестать работать:", err)
	}
	if _, err := e.s.Auth(j.Key); err != nil {
		t.Fatal("выданный ключ должен работать:", err)
	}
	if _, err := e.s.Join(inv2, "Маша"); err != nil {
		t.Fatal(err)
	}
}

func TestReviewerCount(t *testing.T) {
	for eligible, want := range map[int]int{0: 0, 1: 1, 2: 1, 3: 2, 4: 2, 5: 3, 6: 3, 10: 5} {
		if got := reviewerCount(eligible); got != want {
			t.Fatalf("%d подходящих: %d ревьюеров, ждали %d", eligible, got, want)
		}
	}
}

func TestRequestRules(t *testing.T) {
	e := newEnv(t)
	vasya := e.join("Вася")
	if _, err := e.s.CreateRequest(vasya, mr9); !isUserErr(err) || !strings.Contains(err.Error(), "не запущен") {
		t.Fatal("без отметки клиента просить нельзя:", err)
	}
	e.beat(vasya)
	if _, err := e.s.CreateRequest(vasya, mr9); !isUserErr(err) || !strings.Contains(err.Error(), "не подключён") {
		t.Fatal("без подключённого проекта просить нельзя:", err)
	}
	e.beat(vasya, repoKey)
	for _, bad := range []string{mr9 + "/diffs", "https://gitlab.example.com/acme/shop/-/merge_requests/9;id", "ftp://x"} {
		if _, err := e.s.CreateRequest(vasya, bad); !isUserErr(err) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	r, err := e.s.CreateRequest(vasya, mr9)
	if err != nil || !r.Waiting || r.Balance != 1 || e.balance(vasya) != 1 {
		t.Fatal(r, err)
	}
	if _, err := e.s.CreateRequest(vasya, mr9); !isUserErr(err) || !strings.Contains(err.Error(), "уже идёт") {
		t.Fatal("второй запрос по тому же MR, пока идёт первый:", err)
	}
	if _, err := e.s.CreateRequest(vasya, mr10); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.CreateRequest(vasya, "https://gitlab.example.com/acme/shop/-/merge_requests/11"); !isUserErr(err) || !strings.Contains(err.Error(), "баллов нет") {
		t.Fatal("при нулевом балансе просить нельзя:", err)
	}
	e.advance(OnlineWindow + time.Second)
	if _, err := e.s.CreateRequest(vasya, mr9); !isUserErr(err) {
		t.Fatal("клиент пропал — просить нельзя:", err)
	}
}

func TestSelectionHalfOfEligible(t *testing.T) {
	e := newEnv(t)
	vasya := e.join("Вася")
	e.beat(vasya, repoKey)
	var others []Member
	for _, n := range []string{"Аня", "Боря", "Витя", "Галя", "Дима"} {
		m := e.join(n)
		e.beat(m, repoKey)
		others = append(others, m)
	}
	paused := e.join("Пауза")
	if _, err := e.s.Heartbeat(paused, []string{repoKey}, e.now.Add(time.Hour).Unix(), ""); err != nil {
		t.Fatal(err)
	}
	noRepo := e.join("Безпроекта")
	e.beat(noRepo, "gitlab.example.com/acme/api")
	offline := e.join("Офлайн")
	e.beat(offline, repoKey)
	e.advance(OnlineWindow - 10*time.Second)
	for _, m := range append([]Member{vasya, noRepo}, others...) {
		e.beat(m, repoKey)
	}
	if _, err := e.s.Heartbeat(paused, []string{repoKey}, e.now.Add(time.Hour).Unix(), ""); err != nil {
		t.Fatal(err)
	}
	e.advance(20 * time.Second) // Офлайн выпал из окна
	r, err := e.s.CreateRequest(vasya, mr9)
	if err != nil {
		t.Fatal(err)
	}
	// Подходят 5 (Аня…Дима) и Безпроекта, который только что подключил shop; ⌈6/2⌉ = 3.
	if len(r.Reviewers) != 3 || r.Waiting {
		t.Fatal(r)
	}
	for _, name := range r.Reviewers {
		if name == "Вася" || name == "Пауза" || name == "Офлайн" {
			t.Fatal("выбран неподходящий:", r.Reviewers)
		}
	}
	// Следующий запрос достаётся тем, кто ещё не получал заданий, а занятые не участвуют.
	anya := others[0]
	e.beat(anya, repoKey)
	got := map[string]bool{}
	for _, n := range r.Reviewers {
		got[n] = true
	}
	r2, err := e.s.CreateRequest(anya, mr10)
	if err != nil && !got["Аня"] {
		t.Fatal(err)
	}
	if err == nil {
		for _, n := range r2.Reviewers {
			if got[n] {
				t.Fatal("занятый получил второе задание:", n, r2.Reviewers)
			}
		}
	}
}

func TestResultSuccessAndReplacement(t *testing.T) {
	e := newEnv(t)
	vasya, petya, masha := e.join("Вася"), e.join("Петя"), e.join("Маша")
	for _, m := range []Member{vasya, petya, masha} {
		e.beat(m, repoKey)
	}
	r, err := e.s.CreateRequest(vasya, mr9)
	if err != nil || len(r.Reviewers) != 1 {
		t.Fatal(r, err)
	}
	first := petya
	second := masha
	if r.Reviewers[0] == "Маша" {
		first, second = masha, petya
	}
	j := e.take(first)
	if j == nil || j.MRURL != mr9 || j.Requester != "Вася" {
		t.Fatal(j)
	}
	if e.take(second) != nil {
		t.Fatal("второму задание не выдавали")
	}
	if err := e.s.Result(first, j.ID, 3, "boom"); err != nil {
		t.Fatal(err)
	}
	if e.balance(first) != 2 {
		t.Fatal("за неудачу баллов нет")
	}
	j2 := e.take(second)
	if j2 == nil || j2.MRURL != mr9 {
		t.Fatal("упавшее задание должно уйти следующему:", j2)
	}
	if err := e.s.Result(second, j2.ID, 0, ""); err != nil {
		t.Fatal(err)
	}
	if e.balance(second) != 3 || e.balance(vasya) != 1 {
		t.Fatal(e.balance(second), e.balance(vasya))
	}
	if err := e.s.Result(second, j2.ID, 0, ""); !isUserErr(err) {
		t.Fatal("повторный итог не принимается:", err)
	}
	if err := e.s.Result(first, j2.ID, 0, ""); !isUserErr(err) {
		t.Fatal("чужое задание закрыть нельзя:", err)
	}
	reqs, _ := e.s.MyRequests(vasya)
	if len(reqs) != 1 || reqs[0].Status != "done" || len(reqs[0].Jobs) != 2 {
		t.Fatal(reqs)
	}
	b := e.beat(vasya, repoKey)
	text := ""
	for _, ev := range b.Events {
		text += ev.Text + "\n"
	}
	if !strings.Contains(text, "ищу замену") || !strings.Contains(text, "закончил ревью") || !strings.Contains(text, "1 из 2") {
		t.Fatal(text)
	}
	if b2 := e.beat(vasya, repoKey); len(b2.Events) != 0 {
		t.Fatal("уведомления доставляются один раз:", b2.Events)
	}
}

func TestReplacementOnlyOnce(t *testing.T) {
	e := newEnv(t)
	vasya, petya, masha, kolya := e.join("Вася"), e.join("Петя"), e.join("Маша"), e.join("Коля")
	for _, m := range []Member{vasya, petya, masha, kolya} {
		e.beat(m, repoKey)
	}
	if _, err := e.s.CreateRequest(vasya, mr9); err != nil {
		t.Fatal(err)
	}
	fails := 0
	for i := 0; i < 3; i++ {
		for _, m := range []Member{petya, masha, kolya} {
			if j := e.take(m); j != nil {
				if err := e.s.Result(m, j.ID, 1, ""); err != nil {
					t.Fatal(err)
				}
				fails++
			}
		}
	}
	// ⌈3/2⌉ = 2 задания, у каждого одна замена: всего 4 попытки, но ревьюеров всего трое.
	if fails != 3 {
		t.Fatal("попыток:", fails)
	}
	reqs, _ := e.s.MyRequests(vasya)
	if reqs[0].Status != "open" {
		t.Fatal("одна замена ещё ждёт свободного ревьюера:", reqs[0])
	}
}

func TestWaitingRequestAndExpiry(t *testing.T) {
	e := newEnv(t)
	vasya := e.join("Вася")
	e.beat(vasya, repoKey)
	r, err := e.s.CreateRequest(vasya, mr9)
	if err != nil || !r.Waiting {
		t.Fatal(r, err)
	}
	petya := e.join("Петя")
	e.beat(petya, repoKey) // отметка раздаёт ждущие задания
	if j := e.take(petya); j == nil || j.MRURL != mr9 {
		t.Fatal(j)
	}
	if _, err := e.s.CreateRequest(vasya, mr10); err != nil {
		t.Fatal(err)
	}
	e.advance(RequestTTL + time.Minute)
	e.beat(vasya, repoKey)
	if err := e.s.Tick(); err != nil {
		t.Fatal(err)
	}
	reqs, _ := e.s.MyRequests(vasya)
	if reqs[0].MRURL != mr10 || reqs[0].Status != "done" || reqs[0].Jobs[0].Status != "expired" {
		t.Fatal(reqs[0])
	}
	if e.balance(vasya) != 0 {
		t.Fatal("балл за запрос не возвращается")
	}
}

func TestOfflineReviewerReplaced(t *testing.T) {
	e := newEnv(t)
	vasya, petya := e.join("Вася"), e.join("Петя")
	e.beat(vasya, repoKey)
	e.beat(petya, repoKey)
	if _, err := e.s.CreateRequest(vasya, mr9); err != nil {
		t.Fatal(err)
	}
	if e.take(petya) == nil {
		t.Fatal("нет задания")
	}
	masha := e.join("Маша")
	e.advance(OfflineAfter - time.Second)
	e.beat(vasya, repoKey)
	e.beat(masha, repoKey)
	e.advance(2 * time.Second)
	e.beat(masha, repoKey)
	if err := e.s.Tick(); err != nil {
		t.Fatal(err)
	}
	if j := e.take(masha); j == nil {
		t.Fatal("задание пропавшего должно уйти Маше")
	}
}

func TestPauseReleasesAssignedJob(t *testing.T) {
	e := newEnv(t)
	vasya, petya, masha := e.join("Вася"), e.join("Петя"), e.join("Маша")
	for _, m := range []Member{vasya, petya, masha} {
		e.beat(m, repoKey)
	}
	r, _ := e.s.CreateRequest(vasya, mr9)
	who, other := petya, masha
	if r.Reviewers[0] == "Маша" {
		who, other = masha, petya
	}
	if _, err := e.s.Heartbeat(who, []string{repoKey}, e.now.Add(3*time.Hour).Unix(), ""); err != nil {
		t.Fatal(err)
	}
	if j := e.take(who); j != nil {
		t.Fatal("на паузе задание не выдаётся")
	}
	e.beat(other, repoKey)
	if j := e.take(other); j == nil {
		t.Fatal("задание того, кто ушёл на паузу, должно уйти другому")
	}
}

func TestRevokeAndGrant(t *testing.T) {
	e := newEnv(t)
	j, _ := e.s.Join(e.inv, "Петя")
	if err := e.s.Grant("Петя", 5); err != nil {
		t.Fatal(err)
	}
	ms, _ := e.s.Members()
	if len(ms) != 1 || ms[0].Balance != 7 {
		t.Fatal(ms)
	}
	if err := e.s.Revoke("Петя"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Auth(j.Key); !errors.Is(err, ErrBadKey) {
		t.Fatal("отозванный ключ не работает:", err)
	}
	if _, err := e.s.Join(e.inv, "Петя"); err != nil {
		t.Fatal("имя отозванного освобождается:", err)
	}
	if err := e.s.Revoke("Нет такого"); !isUserErr(err) {
		t.Fatal(err)
	}
}

func TestNextJobWaits(t *testing.T) {
	e := newEnv(t)
	petya := e.join("Петя")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	j, err := e.s.NextJob(ctx, petya, 1500*time.Millisecond)
	if err != nil || j != nil || time.Since(start) < time.Second {
		t.Fatal(j, err, time.Since(start))
	}
}
