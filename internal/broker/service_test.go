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

func TestOfferedToAllEligible(t *testing.T) {
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
	e.advance(OnlineWindow + time.Second) // Офлайн выпал из окна
	for _, m := range append([]Member{vasya}, others...) {
		e.beat(m, repoKey)
	}
	e.beat(noRepo, "gitlab.example.com/acme/api")
	e.s.Heartbeat(paused, []string{repoKey}, e.now.Add(time.Hour).Unix(), "")
	r, err := e.s.CreateRequest(vasya, mr9)
	if err != nil {
		t.Fatal(err)
	}
	// Запрос предлагается всем свободным с проектом: не себе, не на паузе, не офлайн, не без проекта.
	if strings.Join(r.Reviewers, ",") != "Аня,Боря,Витя,Галя,Дима" || r.Waiting {
		t.Fatal(r)
	}
	// У каждого одно предложение за раз: второй запрос получает только свободный Вася, остальные —
	// когда освободятся.
	anya := others[0]
	r2, err := e.s.CreateRequest(anya, mr10)
	if err != nil || strings.Join(r2.Reviewers, ",") != "Вася" {
		t.Fatal("занятым второе предложение не приходит:", r2, err)
	}
	boris := others[1]
	j := e.take(boris)
	if j == nil || j.MRURL != mr9 {
		t.Fatal(j)
	}
	if err := e.s.Decline(boris, j.ID); err != nil {
		t.Fatal(err)
	}
	if j2 := e.take(boris); j2 == nil || j2.MRURL != mr10 {
		t.Fatal("освободившийся получает ждущий запрос:", j2)
	}
}

func TestResultSuccessAndFailure(t *testing.T) {
	e := newEnv(t)
	vasya, petya, masha := e.join("Вася"), e.join("Петя"), e.join("Маша")
	for _, m := range []Member{vasya, petya, masha} {
		e.beat(m, repoKey)
	}
	r, err := e.s.CreateRequest(vasya, mr9)
	if err != nil || len(r.Reviewers) != 2 {
		t.Fatal(r, err)
	}
	jp, jm := e.take(petya), e.take(masha)
	if jp == nil || jm == nil || jp.MRURL != mr9 || jp.Requester != "Вася" {
		t.Fatal("предложение пришло обоим:", jp, jm)
	}
	if err := e.s.Result(petya, jp.ID, 3, "boom"); err != nil {
		t.Fatal(err)
	}
	if e.balance(petya) != 2 {
		t.Fatal("за неудачу баллов нет")
	}
	if err := e.s.Result(masha, jm.ID, 0, ""); err != nil {
		t.Fatal(err)
	}
	if e.balance(masha) != 3 || e.balance(vasya) != 1 {
		t.Fatal(e.balance(masha), e.balance(vasya))
	}
	if err := e.s.Result(masha, jm.ID, 0, ""); !isUserErr(err) {
		t.Fatal("повторный итог не принимается:", err)
	}
	if err := e.s.Result(petya, jm.ID, 0, ""); !isUserErr(err) {
		t.Fatal("чужое задание закрыть нельзя:", err)
	}
	reqs, _ := e.s.MyRequests(vasya)
	if len(reqs) != 1 || reqs[0].Status != "done" || len(reqs[0].Jobs) != 2 {
		t.Fatal(reqs)
	}
	text := events(e.beat(vasya, repoKey))
	if !strings.Contains(text, "у Петя ревью не вышло") || !strings.Contains(text, "Маша закончил ревью") || !strings.Contains(text, "1 из 2") {
		t.Fatal(text)
	}
	if strings.Contains(text, "никто не взял") {
		t.Fatal("Маша ещё ревьюила — запрос не простаивал:", text)
	}
	if b2 := e.beat(vasya, repoKey); len(b2.Events) != 0 {
		t.Fatal("уведомления доставляются один раз:", b2.Events)
	}
}

func TestFailedRequestWaitsForNewcomer(t *testing.T) {
	e := newEnv(t)
	vasya, petya := e.join("Вася"), e.join("Петя")
	e.beat(vasya, repoKey)
	e.beat(petya, repoKey)
	e.s.CreateRequest(vasya, mr9)
	j := e.take(petya)
	if err := e.s.Result(petya, j.ID, 1, ""); err != nil {
		t.Fatal(err)
	}
	text := events(e.beat(vasya, repoKey))
	if !strings.Contains(text, "сейчас ревью никто не взял — запрос ждёт до") {
		t.Fatal("просящий узнаёт, что запрос остался без ревьюеров:", text)
	}
	if e.take(petya) != nil {
		t.Fatal("тому, у кого не вышло, тот же запрос второй раз не предлагается")
	}
	reqs, _ := e.s.MyRequests(vasya)
	last := reqs[0].Jobs[len(reqs[0].Jobs)-1]
	if reqs[0].Status != "open" || last.Status != "pending" || last.Reviewer != "" {
		t.Fatal("запрос ждёт:", reqs[0])
	}
	kolya := e.join("Коля")
	e.beat(kolya, repoKey) // пришёл, пока запрос открыт
	j2 := e.take(kolya)
	if j2 == nil || j2.MRURL != mr9 {
		t.Fatal(j2)
	}
	e.s.Result(kolya, j2.ID, 0, "")
	if text := events(e.beat(vasya, repoKey)); !strings.Contains(text, "1 из 2") {
		t.Fatal(text)
	}
}

func TestNewcomerJoinsOpenRequest(t *testing.T) {
	e := newEnv(t)
	vasya, petya := e.join("Вася"), e.join("Петя")
	e.beat(vasya, repoKey)
	e.beat(petya, repoKey)
	e.s.CreateRequest(vasya, mr9)
	jp := e.take(petya)
	e.s.Accept(petya, jp.ID)
	masha := e.join("Маша")
	e.beat(masha, repoKey)
	jm := e.take(masha)
	if jm == nil || jm.MRURL != mr9 {
		t.Fatal("пришедший, пока запрос открыт, тоже получает предложение:", jm)
	}
	e.s.Result(petya, jp.ID, 0, "")
	reqs, _ := e.s.MyRequests(vasya)
	if reqs[0].Status != "open" {
		t.Fatal("пока Маша думает, запрос открыт:", reqs[0])
	}
	e.s.Result(masha, jm.ID, 0, "")
	if text := events(e.beat(vasya, repoKey)); !strings.Contains(text, "2 из 2") {
		t.Fatal(text)
	}
	// Закрытый запрос новым участникам больше не предлагается.
	kolya := e.join("Коля")
	e.beat(kolya, repoKey)
	if e.take(kolya) != nil {
		t.Fatal("закрытый запрос предложен")
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
	if reqs[0].MRURL != mr10 || reqs[0].Status != "done" || len(reqs[0].Jobs) != 0 {
		t.Fatal("за сутки запрос никому не предлагали:", reqs[0])
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

func TestDeclineClosesOnlyOwnOffer(t *testing.T) {
	e := newEnv(t)
	vasya, petya, masha := e.join("Вася"), e.join("Петя"), e.join("Маша")
	for _, m := range []Member{vasya, petya, masha} {
		e.beat(m, repoKey)
	}
	if _, err := e.s.CreateRequest(vasya, mr9); err != nil {
		t.Fatal(err)
	}
	jp, jm := e.take(petya), e.take(masha)
	if err := e.s.Decline(masha, jp.ID); !isUserErr(err) {
		t.Fatal("отказаться от чужого задания нельзя:", err)
	}
	if err := e.s.Decline(petya, jp.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.s.Decline(petya, jp.ID); !isUserErr(err) {
		t.Fatal("повторный отказ:", err)
	}
	if err := e.s.Result(petya, jp.ID, 0, ""); !isUserErr(err) {
		t.Fatal("за отклонённое задание балла нет:", err)
	}
	if e.take(petya) != nil {
		t.Fatal("отказавшемуся тот же запрос больше не предлагается")
	}
	if text := events(e.beat(vasya, repoKey)); text != "" {
		t.Fatal("пока Маша думает, об отказе Пети просящему не пишем:", text)
	}
	if err := e.s.Accept(masha, jm.ID); err != nil {
		t.Fatal(err)
	}
	e.s.Result(masha, jm.ID, 0, "")
	text := events(e.beat(vasya, repoKey))
	// Итог считает тех, кто брался за ревью: отказ не в счёт.
	if !strings.Contains(text, "Маша взял ревью") || !strings.Contains(text, "1 из 1") {
		t.Fatal(text)
	}
	reqs, _ := e.s.MyRequests(vasya)
	if got := reqs[0].Jobs[0].Status + "," + reqs[0].Jobs[1].Status; got != "declined,done" {
		t.Fatal(got)
	}
}

func TestEveryoneDeclinedRequestWaits(t *testing.T) {
	e := newEnv(t)
	vasya, petya := e.join("Вася"), e.join("Петя")
	e.beat(vasya, repoKey)
	e.beat(petya, repoKey)
	e.s.CreateRequest(vasya, mr9)
	e.s.Decline(petya, e.take(petya).ID)
	text := events(e.beat(vasya, repoKey))
	if strings.Count(text, "сейчас ревью никто не взял") != 1 {
		t.Fatal("все отказались — просящему один раз пишем, что запрос ждёт:", text)
	}
	reqs, _ := e.s.MyRequests(vasya)
	if reqs[0].Status != "open" {
		t.Fatal(reqs[0])
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
	if _, err := e.s.CreateRequest(ms["Вася"], mr9); err != nil {
		t.Fatal(err)
	}
	b := e.beat(ms["Вася"], repoKey)
	var online []string
	offered := 0
	for _, tm := range b.Team.Online {
		online = append(online, tm.Name+":"+tm.State)
		if tm.State == "offered" {
			offered++
		}
	}
	got := strings.Join(online, " ")
	// По имени без учёта регистра; Офлайн выпал из окна, Ушёл отозван, Дима не отмечался.
	for i, want := range []string{"аня:", "Вася:free", "Коля:", "Маша:paused", "Петя:"} {
		if !strings.HasPrefix(online[i], want) {
			t.Fatal(got)
		}
	}
	if len(online) != 5 || offered != 3 { // запрос предложен всем свободным: ане, Коле, Пете
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

func TestOfferedThenReviewingState(t *testing.T) {
	e := newEnv(t)
	vasya, petya, old := e.join("Вася"), e.join("Петя"), e.join("Старый")
	e.beat(vasya, repoKey)
	e.beat(petya, repoKey)
	if _, err := e.s.Heartbeat(old, []string{repoKey}, 0, "v0.1.3"); err != nil {
		t.Fatal(err)
	}
	e.s.CreateRequest(vasya, mr9)
	jp := e.take(petya)
	e.take(old)
	state := func() string {
		var out []string
		for _, tm := range e.beat(vasya, repoKey).Team.Online {
			out = append(out, tm.Name+":"+tm.State)
		}
		return strings.Join(out, " ")
	}
	// Клиент до v0.1.4 не спрашивает «принять?»: взятое им задание — уже ревью.
	if got := state(); got != "Вася:free Петя:offered Старый:busy" {
		t.Fatal(got)
	}
	reqs, _ := e.s.MyRequests(vasya)
	if got := reqs[0].Jobs[0].Status + "," + reqs[0].Jobs[1].Status; got != "offered,running" {
		t.Fatal(got)
	}
	e.s.Accept(petya, jp.ID)
	if got := state(); got != "Вася:free Петя:busy Старый:busy" {
		t.Fatal("после «Принять» — ревьюит:", got)
	}
}

func TestPausedOfferWithdrawnSilently(t *testing.T) {
	e := newEnv(t)
	vasya, petya := e.join("Вася"), e.join("Петя")
	e.beat(vasya, repoKey)
	e.beat(petya, repoKey)
	e.s.CreateRequest(vasya, mr9)
	if _, err := e.s.Heartbeat(petya, []string{repoKey}, e.now.Add(time.Hour).Unix(), ""); err != nil {
		t.Fatal(err)
	}
	text := events(e.beat(vasya, repoKey))
	if strings.Contains(text, "не вышло") || !strings.Contains(text, "сейчас ревью никто не взял") {
		t.Fatal("предложение снято без «не вышло», запрос ждёт:", text)
	}
	reqs, _ := e.s.MyRequests(vasya)
	if len(reqs[0].Jobs) != 1 || reqs[0].Jobs[0].Status != "pending" {
		t.Fatal("снятое предложение не показывается:", reqs[0].Jobs)
	}
}

func TestAcceptedAtAddedToOldDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE jobs DROP COLUMN accepted_at`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = OpenDB(path)
	if err != nil {
		t.Fatal("база v0.1.4 открывается:", err)
	}
	defer db.Close()
	var n int
	db.QueryRow(`SELECT count(*) FROM pragma_table_info('jobs') WHERE name = 'accepted_at'`).Scan(&n)
	if n != 1 {
		t.Fatal("колонка accepted_at не добавлена")
	}
}

func TestLegacyClient(t *testing.T) {
	for v, want := range map[string]bool{"v0.1.3": true, "v0.0.9": true, "v0.1.4": false, "v0.2.0": false, "v1.0.0": false, "dev": false, "?": false, "": false} {
		if legacyClient(v) != want {
			t.Errorf("%q: %v", v, !want)
		}
	}
}

func TestUntakenOfferWithdrawnAfterDay(t *testing.T) {
	e := newEnv(t)
	vasya, petya := e.join("Вася"), e.join("Петя")
	e.beat(vasya, repoKey)
	e.beat(petya, repoKey)
	e.s.CreateRequest(vasya, mr9)
	// Петя онлайн, но предложение так и не забрал: через сутки оно снимается, запрос закрывается.
	for i := 0; i < 25; i++ {
		e.advance(time.Hour)
		e.beat(vasya, repoKey)
		e.beat(petya, repoKey)
	}
	if err := e.s.Tick(); err != nil {
		t.Fatal(err)
	}
	reqs, _ := e.s.MyRequests(vasya)
	if reqs[0].Status != "done" || len(reqs[0].Jobs) != 0 {
		t.Fatal(reqs[0])
	}
	if e.take(petya) != nil {
		t.Fatal("снятое предложение не выдаётся")
	}
	if text := events(e.beat(vasya, repoKey)); !strings.Contains(text, "за сутки ревью никто не взял") {
		t.Fatal(text)
	}
}
