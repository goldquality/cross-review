package broker

import (
	"context"
	"errors"
	"fmt"
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
	for _, r := range []string{repoKey, "gitlab.example.com/acme/api"} {
		if _, err := e.s.AddRepo(r); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

// invite — личное приглашение на имя, как его выдаёт администратор.
func (e *env) invite(name string) string {
	e.t.Helper()
	inv, err := e.s.CreateInvite(name)
	if err != nil {
		e.t.Fatal(err)
	}
	return inv.Code
}

func (e *env) joinKey(name string) Joined {
	e.t.Helper()
	j, err := e.s.Join(e.invite(name))
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

func (e *env) join(name string) Member {
	e.t.Helper()
	m, err := e.s.Auth(e.joinKey(name).Key)
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

func TestPersonalInvites(t *testing.T) {
	e := newEnv(t)
	inv := e.invite("Петя")
	if !strings.HasPrefix(inv, "inv_") {
		t.Fatal(inv)
	}
	j, err := e.s.Join(inv)
	if err != nil || j.Name != "Петя" || j.Balance != StartBalance || !strings.HasPrefix(j.Key, "crk_") {
		t.Fatal(j, err)
	}
	if _, err := e.s.Join(inv); !errors.Is(err, ErrBadInvite) {
		t.Fatal("приглашение одноразовое:", err)
	}
	for _, bad := range []string{"", "inv_wrong", "crk_x"} {
		if _, err := e.s.Join(bad); !errors.Is(err, ErrBadInvite) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	// Новое приглашение на имя отменяет прежнее; просроченное не работает.
	old := e.invite("Маша")
	fresh := e.invite("Маша")
	if _, err := e.s.Join(old); !errors.Is(err, ErrBadInvite) {
		t.Fatal("прежнее приглашение на имя отменяется:", err)
	}
	e.advance(InviteTTL + time.Second)
	if _, err := e.s.Join(fresh); !errors.Is(err, ErrBadInvite) {
		t.Fatal("просроченное приглашение:", err)
	}
	list, _ := e.s.Invites()
	if len(list) != 0 {
		t.Fatal("в списке только действующие:", list)
	}
	// Имена: без смеси алфавитов и без двойников на глаз.
	for _, bad := range []string{"", " ", "a;b", "<script>", strings.Repeat("я", 41), "Аlice", "Pеtya"} {
		if _, err := e.s.CreateInvite(bad); !isUserErr(err) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	e.joinKey("Alice")
	if _, err := e.s.CreateInvite("Аliсе"); !isUserErr(err) {
		t.Fatal("смесь алфавитов:", err)
	}
	if _, err := e.s.CreateInvite("Аое"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.CreateInvite("Aoe"); !isUserErr(err) {
		t.Fatal("кириллическое «Аое» и латинское «Aoe» не отличить:", err)
	}
	if _, err := e.s.Auth("crk_nope"); !errors.Is(err, ErrBadKey) {
		t.Fatal(err)
	}
}

func TestReinviteRekeysAndKeepsBalance(t *testing.T) {
	e := newEnv(t)
	j := e.joinKey("Петя")
	if err := e.s.Grant("петя", 5); err != nil {
		t.Fatal(err)
	}
	if err := e.s.Revoke("Петя"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Auth(j.Key); !errors.Is(err, ErrBadKey) {
		t.Fatal("отозванный ключ не работает:", err)
	}
	// Имя отозванного не освобождается: занять его можно только приглашением администратора, и
	// тогда это тот же участник с прежними баллами.
	inv, err := e.s.CreateInvite("Петя")
	if err != nil || !inv.Rekey {
		t.Fatal(inv, err)
	}
	j2, err := e.s.Join(inv.Code)
	if err != nil || j2.Balance != 7 || j2.Key == j.Key {
		t.Fatal(j2, err)
	}
	if _, err := e.s.Auth(j2.Key); err != nil {
		t.Fatal(err)
	}
	ms, _ := e.s.Members()
	if len(ms) != 1 || ms[0].Name != "Петя" || ms[0].Balance != 7 {
		t.Fatal("тот же участник, без дубля:", ms)
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
	j := e.joinKey("Петя")
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

func events(b Beat) string {
	text := ""
	for _, ev := range b.Events {
		text += ev.Text + "\n"
	}
	return text
}

func TestDeclinePassesToNext(t *testing.T) {
	e := newEnv(t)
	vasya, petya, masha := e.join("Вася"), e.join("Петя"), e.join("Маша")
	for _, m := range []Member{vasya, petya, masha} {
		e.beat(m, repoKey)
	}
	r, err := e.s.CreateRequest(vasya, mr9)
	if err != nil || len(r.Reviewers) != 1 {
		t.Fatal(r, err)
	}
	first, second := petya, masha
	if r.Reviewers[0] == "Маша" {
		first, second = masha, petya
	}
	j := e.take(first)
	if err := e.s.Decline(second, j.ID); !isUserErr(err) {
		t.Fatal("отказаться от чужого задания нельзя:", err)
	}
	if err := e.s.Decline(first, j.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.s.Decline(first, j.ID); !isUserErr(err) {
		t.Fatal("повторный отказ:", err)
	}
	if err := e.s.Result(first, j.ID, 0, ""); !isUserErr(err) {
		t.Fatal("за отклонённое задание балла нет:", err)
	}
	j2 := e.take(second)
	if j2 == nil || j2.MRURL != mr9 {
		t.Fatal("после отказа задание уходит следующему:", j2)
	}
	// Отказ не тратит замену: упавшее у второго задание ещё раз ищет ревьюера.
	if err := e.s.Result(second, j2.ID, 1, ""); err != nil {
		t.Fatal(err)
	}
	if e.take(first) != nil {
		t.Fatal("отказавшемуся задание по этому запросу больше не приходит")
	}
	kolya := e.join("Коля")
	e.beat(kolya, repoKey)
	j3 := e.take(kolya)
	if j3 == nil || j3.MRURL != mr9 {
		t.Fatal("замена после неудачи:", j3)
	}
	if err := e.s.Result(kolya, j3.ID, 0, ""); err != nil {
		t.Fatal(err)
	}
	text := events(e.beat(vasya, repoKey))
	// Итог считает тех, кто брался за ревью: отказ не в счёт.
	if !strings.Contains(text, first.Name+" отказался, ищу замену") || !strings.Contains(text, "1 из 2") {
		t.Fatal(text)
	}
	if e.balance(first) != 2 || e.balance(kolya) != 3 {
		t.Fatal(e.balance(first), e.balance(kolya))
	}
}

func TestAcceptStartsTheClock(t *testing.T) {
	e := newEnv(t)
	vasya, petya := e.join("Вася"), e.join("Петя")
	e.beat(vasya, repoKey)
	e.beat(petya, repoKey)
	if _, err := e.s.CreateRequest(vasya, mr9); err != nil {
		t.Fatal(err)
	}
	j := e.take(petya)
	if err := e.s.Accept(vasya, j.ID); !isUserErr(err) {
		t.Fatal("принять чужое задание нельзя:", err)
	}
	e.advance(20 * time.Minute) // человек думал
	if err := e.s.Accept(petya, j.ID); err != nil {
		t.Fatal(err)
	}
	if text := events(e.beat(vasya, repoKey)); !strings.Contains(text, "shop!9: Петя взял ревью") {
		t.Fatal(text)
	}
	// 20 минут раздумий + 50 минут ревью больше 65, но время пошло с принятия.
	for i := 0; i < 5; i++ {
		e.advance(10 * time.Minute)
		e.beat(petya, repoKey)
		e.beat(vasya, repoKey)
	}
	if err := e.s.Tick(); err != nil {
		t.Fatal(err)
	}
	if err := e.s.Result(petya, j.ID, 0, ""); err != nil {
		t.Fatal("задание не должно было истечь:", err)
	}
	if err := e.s.Accept(petya, j.ID); !isUserErr(err) {
		t.Fatal("закрытое задание не принимается:", err)
	}
}

func TestExpiredWithoutReviewers(t *testing.T) {
	e := newEnv(t)
	vasya := e.join("Вася")
	e.beat(vasya, repoKey)
	if _, err := e.s.CreateRequest(vasya, mr9); err != nil {
		t.Fatal(err)
	}
	e.advance(RequestTTL + time.Minute)
	e.beat(vasya, repoKey)
	if err := e.s.Tick(); err != nil {
		t.Fatal(err)
	}
	if text := events(e.beat(vasya, repoKey)); !strings.Contains(text, "за сутки ревью никто не взял") {
		t.Fatal(text)
	}
}

func TestTeamInHeartbeat(t *testing.T) {
	e := newEnv(t)
	names := []string{"Вася", "аня", "Петя", "Маша", "Коля", "Дима", "Офлайн", "Ушёл"}
	ms := map[string]Member{}
	for _, n := range names {
		ms[n] = e.join(n)
	}
	e.s.Grant("Петя", 5)   // 7
	e.s.Grant("Офлайн", 4) // 6
	e.s.Grant("Ушёл", 9)   // отозван — нигде не показывается
	e.s.Grant("Маша", 1)   // 3
	e.s.Grant("Коля", 1)   // 3
	e.beat(ms["Офлайн"], repoKey)
	e.advance(OnlineWindow + time.Second)
	for _, n := range []string{"Вася", "аня", "Петя", "Маша", "Коля", "Ушёл"} {
		e.beat(ms[n], repoKey)
	}
	if err := e.s.Revoke("Ушёл"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Heartbeat(ms["Маша"], []string{repoKey}, e.now.Add(time.Hour).Unix(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.CreateRequest(ms["Вася"], mr9); err != nil { // задания получат двое из свободных
		t.Fatal(err)
	}
	b := e.beat(ms["Вася"], repoKey)
	var online []string
	busy := 0
	for _, tm := range b.Team.Online {
		online = append(online, tm.Name+":"+tm.State)
		if tm.State == "busy" {
			busy++
		}
	}
	got := strings.Join(online, " ")
	// По имени без учёта регистра; Офлайн выпал из окна, Ушёл отозван, Дима не отмечался.
	for i, want := range []string{"аня:", "Вася:free", "Коля:", "Маша:paused", "Петя:"} {
		if !strings.HasPrefix(online[i], want) {
			t.Fatal(got)
		}
	}
	if len(online) != 5 || busy != 2 { // свободны аня, Коля, Петя: ⌈3/2⌉ = 2
		t.Fatal("онлайн:", got)
	}
	var top []string
	for _, tm := range b.Team.Top {
		top = append(top, tm.Name+"="+itoa(tm.Balance))
		if tm.State != "" {
			t.Fatal("в топе состояние не нужно:", tm)
		}
	}
	// Вася потратил балл на запрос. При равенстве — по имени.
	if g := strings.Join(top, " "); g != "Петя=7 Офлайн=6 Коля=3 Маша=3 аня=2" {
		t.Fatal(g)
	}
}

func TestReposAllowlist(t *testing.T) {
	e := newEnv(t)
	vasya, petya := e.join("Вася"), e.join("Петя")
	b := e.beat(vasya, repoKey, "evil.example/x/y")
	if len(b.Ignored) != 1 || b.Ignored[0] != "evil.example/x/y" {
		t.Fatal("не разрешённый репо отмечается, задания по нему не идут:", b.Ignored)
	}
	e.beat(petya, repoKey, "evil.example/x/y")
	if _, err := e.s.CreateRequest(vasya, "https://evil.example/x/y/-/merge_requests/1"); !isUserErr(err) {
		t.Fatal("запрос по не разрешённому репо:", err)
	}
	if r, err := e.s.AddRepo("git@EVIL.example:X/y.git"); err != nil || r != "evil.example/x/y" {
		t.Fatal(r, err)
	}
	if b := e.beat(vasya, repoKey, "evil.example/x/y"); len(b.Ignored) != 0 {
		t.Fatal(b.Ignored)
	}
	// Запрет действует сразу: участники перестают получать задания по репо, не дожидаясь отметки.
	if _, err := e.s.RemoveRepo("evil.example/x/y"); err != nil {
		t.Fatal(err)
	}
	if r, err := e.s.CreateRequest(vasya, "https://evil.example/x/y/-/merge_requests/1"); !isUserErr(err) {
		t.Fatal("после запрета:", r, err)
	}
	if _, err := e.s.RemoveRepo("evil.example/x/y"); !isUserErr(err) {
		t.Fatal("повторный запрет:", err)
	}
	if list, _ := e.s.Repos(); len(list) != 2 {
		t.Fatal(list)
	}
}

func TestReposSeededFromActivationsOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	// База до списка репо: участник уже подключил проект.
	for _, q := range []string{
		`DELETE FROM settings WHERE key = 'repos_seeded'`,
		`INSERT INTO settings(key, value) VALUES('invite', 'inv_shared')`,
		`INSERT INTO members(id, name, name_key, key_hash, created_at) VALUES(1, 'Вася', 'вася', 'h', 0)`,
		`INSERT INTO activations(member_id, repo) VALUES(1, 'gitlab.example.com/acme/shop')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	db, err = OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	s := NewService(db)
	if list, _ := s.Repos(); len(list) != 1 || list[0] != repoKey {
		t.Fatal("подключённое до обновления разрешается:", list)
	}
	if v, _ := s.setting("invite"); v != "" {
		t.Fatal("общее приглашение удаляется из базы:", v)
	}
	s.RemoveRepo(repoKey)
	db.Exec(`INSERT INTO activations(member_id, repo) VALUES(1, 'gitlab.example.com/acme/shop')`)
	db.Close()
	db, _ = OpenDB(path)
	defer db.Close()
	if list, _ := NewService(db).Repos(); len(list) != 0 {
		t.Fatal("дальше список ведёт администратор:", list)
	}
}

func TestVersionSanitized(t *testing.T) {
	e := newEnv(t)
	m := e.join("Петя")
	if _, err := e.s.Heartbeat(m, nil, 0, "\x1b[1A\x1b[2Kv1"); err != nil {
		t.Fatal(err)
	}
	ms, _ := e.s.Members()
	if ms[0].Version != "?" {
		t.Fatalf("версия с управляющими символами: %q", ms[0].Version)
	}
	e.s.Heartbeat(m, nil, 0, "v0.1.4")
	if ms, _ := e.s.Members(); ms[0].Version != "v0.1.4" {
		t.Fatal(ms[0].Version)
	}
}

func TestOpenRequestsCap(t *testing.T) {
	e := newEnv(t)
	vasya := e.join("Вася")
	e.s.Grant("Вася", 10)
	e.beat(vasya, repoKey)
	for i := 1; i <= MaxOpenRequests; i++ {
		if _, err := e.s.CreateRequest(vasya, fmt.Sprintf("https://gitlab.example.com/acme/shop/-/merge_requests/%d", i)); err != nil {
			t.Fatal(i, err)
		}
	}
	if _, err := e.s.CreateRequest(vasya, "https://gitlab.example.com/acme/shop/-/merge_requests/99"); !isUserErr(err) {
		t.Fatal("запросов в работе не больше MaxOpenRequests:", err)
	}
}

func TestOnePollPerMemberAndWake(t *testing.T) {
	e := newEnv(t)
	vasya, petya := e.join("Вася"), e.join("Петя")
	e.beat(vasya, repoKey)
	e.beat(petya, repoKey)
	first := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		e.s.NextJob(context.Background(), petya, 20*time.Second)
		first <- time.Since(start)
	}()
	time.Sleep(100 * time.Millisecond)
	got := make(chan *Job, 1)
	go func() {
		j, _ := e.s.NextJob(context.Background(), petya, 20*time.Second)
		got <- j
	}()
	select {
	case d := <-first:
		if d > 2*time.Second {
			t.Fatal(d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("второй опрос того же участника должен отменить первый")
	}
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	if _, err := e.s.CreateRequest(vasya, mr9); err != nil {
		t.Fatal(err)
	}
	select {
	case j := <-got:
		if j == nil || j.MRURL != mr9 || time.Since(start) > time.Second {
			t.Fatal("ждущий опрос просыпается от нового задания сразу:", j, time.Since(start))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("опрос не проснулся")
	}
}

func TestSnapshotAndEventsPruned(t *testing.T) {
	e := newEnv(t)
	vasya, petya := e.join("Вася"), e.join("Петя")
	e.beat(vasya, repoKey)
	e.beat(petya, repoKey)
	e.s.CreateRequest(vasya, mr9)
	j := e.take(petya)
	e.s.Result(petya, j.ID, 0, "")
	e.beat(vasya, repoKey) // уведомления доставлены
	path := filepath.Join(t.TempDir(), "snap.db")
	if err := e.s.Snapshot(path); err != nil {
		t.Fatal(err)
	}
	if err := e.s.Snapshot(path); err != nil {
		t.Fatal("снимок перезаписывается:", err)
	}
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	db.QueryRow(`SELECT count(*) FROM members`).Scan(&n)
	if n != 2 {
		t.Fatal("в снимке участники:", n)
	}
	count := func() int {
		var c int
		e.s.db.QueryRow(`SELECT count(*) FROM events`).Scan(&c)
		return c
	}
	before := count()
	e.advance(eventsKeep + time.Hour)
	if err := e.s.Tick(); err != nil {
		t.Fatal(err)
	}
	if before == 0 || count() != 0 {
		t.Fatal("доставленные уведомления старше 30 дней удаляются:", before, count())
	}
}
