package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/goldquality/cross-review/internal/mrurl"
)

// Notifier показывает уведомление участнику: в трее — системное, в daemon — строкой в лог.
type Notifier interface {
	Notify(title, text string)
}

// Running — задание, которое выполняется сейчас.
type Running struct {
	MR        string
	Short     string
	Requester string
	Started   time.Time
	LogPath   string
}

// State — что показывать в трее.
type State struct {
	Configured  bool // есть ключ брокера
	Name        string
	Settings    string // файл настроек
	SettingsErr string // что не так с файлом настроек
	Online      bool
	Balance     int
	PausedUntil time.Time
	Current     *Running
	LastError   string
	LastLog     string
	Update      string   // вышла версия новее этой, пусто — обновляться не нужно
	Team        Team     // кто онлайн и топ по баллам, по последней отметке
	Ignored     []string // репо из настроек, которых брокер не разрешает
}

// Сколько уведомлений от брокера показывать за одну отметку: остальное — одной строкой.
const maxEventsShown = 5

// Пауза перед повтором отправки итога и после задания, которое клиент отклонил. Переменные — для тестов.
var (
	retryBase   = 5 * time.Second
	retryFor    = 10 * time.Minute
	rejectPause = 30 * time.Second
)

// OfferTimeout — сколько ждать ответа на задание, потом оно уходит другому.
const OfferTimeout = 5 * time.Minute

// JobOffer — задание, на которое участник отвечает «принять» или «отклонить».
type JobOffer struct {
	MR        mrurl.MR
	Requester string
	Command   string // команда ревью с <LINK>, для показа
	Workdir   string
	Until     time.Time // не ответит до этого времени — задание уйдёт другому
}

// Agent держит связь с брокером: отметки, задания, запросы.
type Agent struct {
	Store     *Store
	Notify    Notifier
	LogDir    string
	Version   string
	ShellPath string
	OnChange  func()
	Now       func() time.Time
	// Ask спрашивает участника, берёт ли он задание; ctx истекает через AskTimeout. nil — задание
	// берётся без вопроса: так работает daemon, где спросить некого.
	Ask        func(ctx context.Context, o JobOffer) bool
	AskTimeout time.Duration // 0 — OfferTimeout

	mu          sync.Mutex
	state       State
	cancel      context.CancelFunc
	kick        chan struct{}
	started     bool
	toldNews    string // о какой новой версии уже сказали
	toldIgnored string // о каких не разрешённых репо уже сказали
}

func (a *Agent) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Agent) api() *API {
	c := a.Store.Get()
	return NewAPI(c.Broker, c.Key)
}

// State — снимок для трея.
func (a *Agent) State() State {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.state
	c := a.Store.Get()
	s.Configured = c.Broker != "" && c.Key != ""
	s.Name = c.Name
	s.Settings, s.SettingsErr = c.Path, c.Err
	if c.PausedUntil > a.now().Unix() {
		s.PausedUntil = time.Unix(c.PausedUntil, 0)
	} else {
		s.PausedUntil = time.Time{}
	}
	if s.Current != nil {
		cur := *s.Current
		s.Current = &cur
	}
	return s
}

func (a *Agent) set(fn func(*State)) {
	a.mu.Lock()
	fn(&a.state)
	a.mu.Unlock()
	if a.OnChange != nil {
		a.OnChange()
	}
}

// Kick — отметиться сейчас, не дожидаясь 30 секунд: после паузы, правки настроек, входа.
func (a *Agent) Kick() {
	a.mu.Lock()
	k := a.kick
	a.mu.Unlock()
	if k != nil {
		select {
		case k <- struct{}{}:
		default:
		}
	}
}

// Run работает, пока жив ctx.
func (a *Agent) Run(ctx context.Context) {
	a.mu.Lock()
	if a.started {
		a.mu.Unlock()
		return
	}
	a.started = true
	a.kick = make(chan struct{}, 1)
	a.mu.Unlock()
	go a.jobLoop(ctx)
	a.heartbeatLoop(ctx)
}

func (a *Agent) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		a.beat(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.kick:
		}
	}
}

func (a *Agent) beat(ctx context.Context) {
	c := a.Store.Get()
	if c.Broker == "" {
		return
	}
	if c.Key == "" {
		if err := a.join(ctx, c); err != nil {
			if ctx.Err() == nil {
				a.set(func(s *State) { s.Online = false; s.LastError = err.Error() })
			}
			return
		}
		c = a.Store.Get()
	}
	paused := c.PausedUntil
	if paused <= a.now().Unix() {
		paused = 0
	}
	b, err := a.api().Heartbeat(ctx, c.Repos(), paused, a.Version)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		msg := err.Error()
		if Unauthorized(err) {
			msg = "ключ отозван или не подходит: спроси администратора брокера"
		}
		a.set(func(s *State) { s.Online = false; s.LastError = msg })
		return
	}
	update := ""
	if NewerVersion(b.Version, a.Version) {
		update = b.Version
	}
	a.set(func(s *State) {
		s.Online = true
		s.Balance = b.Balance
		s.LastError = ""
		s.Update = update
		s.Team = b.Team
		s.Ignored = b.Ignored
	})
	if update != "" && update != a.toldNews {
		a.toldNews = update
		a.Notify.Notify("cross-review", fmt.Sprintf("Вышла cross-review %s, у тебя %s. Скачать: %s", update, a.Version, ReleasesURL))
	}
	if ign := strings.Join(b.Ignored, ", "); ign != a.toldIgnored {
		a.toldIgnored = ign
		if ign != "" {
			a.Notify.Notify("cross-review", "Брокер не разрешает "+ign+": по ним задания не придут. Попроси администратора добавить.")
		}
	}
	for i, e := range b.Events {
		if i == maxEventsShown-1 && len(b.Events) > maxEventsShown {
			a.Notify.Notify("cross-review", fmt.Sprintf("И ещё %d уведомлений — смотри «Мои запросы»", len(b.Events)-i))
			break
		}
		a.Notify.Notify("cross-review", e.Text)
	}
}

// join меняет приглашение из настроек на личный ключ: при первом запуске и после смены брокера.
func (a *Agent) join(ctx context.Context, c Config) error {
	if c.Invite == "" {
		return fmt.Errorf("в настройках нет invite: его даёт администратор брокера")
	}
	j, err := NewAPI(c.Broker, "").Join(ctx, c.Invite, c.Name)
	if err != nil {
		return fmt.Errorf("подключение к брокеру: %w", err)
	}
	if err := a.Store.updateState(func(s *state) { s.Broker, s.Key, s.Name = c.Broker, j.Key, j.Name }); err != nil {
		return err
	}
	a.Notify.Notify("cross-review", fmt.Sprintf("Подключено к брокеру как %s, баланс %d", j.Name, j.Balance))
	return nil
}

func (a *Agent) jobLoop(ctx context.Context) {
	rev := -1
	for ctx.Err() == nil {
		c := a.Store.Get()
		if c.Rev != rev {
			// настройки поменяли: брокер, проекты, приглашение — отметиться сразу
			rev = c.Rev
			a.Kick()
		}
		st := a.State()
		if c.Broker == "" || c.Key == "" || c.Paused(a.now()) || !st.Online {
			sleep(ctx, 5*time.Second)
			continue
		}
		job, err := a.api().NextJob(ctx, 30*time.Second)
		if err != nil {
			sleep(ctx, 10*time.Second)
			continue
		}
		if job != nil {
			a.handle(ctx, *job)
		}
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// handle проверяет ссылку от брокера по своим проектам и запускает свою команду.
func (a *Agent) handle(ctx context.Context, job Job) {
	api := a.api()
	report := func(r Result) {
		a.report(ctx, api, job.ID, r)
		a.Kick()
	}
	mr, err := mrurl.ParseMR(job.MRURL)
	if err != nil {
		report(Result{ExitCode: -2, Tail: "клиент отклонил ссылку: " + err.Error()})
		a.Notify.Notify("cross-review", "Брокер прислал негодную ссылку, задание отклонено")
		sleep(ctx, rejectPause) // брокер, который шлёт мусор, не завалит участника уведомлениями
		return
	}
	c := a.Store.Get()
	p := c.ProjectFor(mr.Repo.String())
	if p == nil {
		report(Result{ExitCode: -2, Tail: "проект " + mr.Repo.String() + " не подключён у ревьюера"})
		sleep(ctx, rejectPause)
		return
	}
	if a.Ask != nil && !a.ask(ctx, api, job, mr, p) {
		return
	}
	jctx, cancel := context.WithCancel(ctx)
	a.mu.Lock()
	a.cancel = cancel
	a.mu.Unlock()
	defer func() {
		cancel()
		a.mu.Lock()
		a.cancel = nil
		a.mu.Unlock()
	}()
	cur := &Running{MR: mr.String(), Short: mr.Short(), Requester: job.Requester, Started: a.now()}
	a.set(func(s *State) { s.Current = cur })
	a.Notify.Notify("cross-review", fmt.Sprintf("Ревьюю %s, просит %s", mr.Short(), job.Requester))
	res, err := Run(jctx, *p, mr, a.LogDir, ChildEnv(a.ShellPath))
	if err != nil {
		res = Result{ExitCode: -1, Tail: err.Error()}
	}
	a.set(func(s *State) { s.Current = nil; s.LastLog = res.LogPath })
	report(res)
	if res.ExitCode == 0 {
		a.Notify.Notify("cross-review", fmt.Sprintf("%s: ревью готово, +1 балл", mr.Short()))
	} else {
		a.Notify.Notify("cross-review", fmt.Sprintf("%s: ревью не вышло (код %d), лог в меню", mr.Short(), res.ExitCode))
	}
}

// report отправляет итог задания и повторяет, пока брокер недоступен: иначе задание провисело бы у
// брокера час и ушло на второе ревью, а ревьюер остался бы без балла. Отказ брокера (4xx) не
// повторяется, выход из клиента прерывает повторы.
func (a *Agent) report(ctx context.Context, api *API, jobID int64, r Result) {
	deadline := time.Now().Add(retryFor)
	delay := retryBase
	for {
		rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := api.Result(rctx, jobID, r)
		cancel()
		if err == nil {
			return
		}
		var be *BrokerError
		final := errors.As(err, &be) && be.Status < 500 && be.Status != http.StatusTooManyRequests
		if final || time.Now().After(deadline) || ctx.Err() != nil {
			a.set(func(s *State) { s.LastError = "итог не отправлен: " + err.Error() })
			return
		}
		a.set(func(s *State) { s.LastError = "итог не отправлен, повторю: " + err.Error() })
		sleep(ctx, delay)
		delay = min(delay*2, time.Minute)
	}
}

// ask спрашивает участника, берёт ли он задание. Отказ, закрытое окно, выход из клиента и молчание
// дольше AskTimeout отдают задание другому.
func (a *Agent) ask(ctx context.Context, api *API, job Job, mr mrurl.MR, p *Project) bool {
	timeout := a.AskTimeout
	if timeout <= 0 {
		timeout = OfferTimeout
	}
	octx, cancel := context.WithTimeout(ctx, timeout)
	ok := a.Ask(octx, JobOffer{MR: mr, Requester: job.Requester, Command: FormatCommand(p.Command), Workdir: p.Workdir, Until: a.now().Add(timeout)})
	timedOut := errors.Is(octx.Err(), context.DeadlineExceeded)
	cancel()
	if ok && ctx.Err() == nil {
		err := api.Accept(ctx, job.ID)
		var be *BrokerError
		// 404 — брокер старше клиента и не знает «принять»: задание и так за участником. Без связи
		// тоже работаем: итог дойдёт, когда она вернётся.
		if errors.As(err, &be) && be.Status != http.StatusNotFound {
			a.Notify.Notify("cross-review", fmt.Sprintf("%s: задание уже не твоё — %s", mr.Short(), be.Msg))
			return false
		}
		return true
	}
	rctx, rcancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer rcancel()
	if err := api.Decline(rctx, job.ID); err != nil {
		a.set(func(s *State) { s.LastError = "отказ не отправлен: " + err.Error() })
	}
	a.Kick()
	if timedOut {
		a.Notify.Notify("cross-review", fmt.Sprintf("%s: ответа не было, задание ушло другому", mr.Short()))
	}
	return false
}

// StopCurrent останавливает текущее задание; оно уйдёт другому ревьюеру.
func (a *Agent) StopCurrent() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel == nil {
		return false
	}
	a.cancel()
	return true
}

// SetPause ставит паузу на d; d = 0 снимает её.
func (a *Agent) SetPause(d time.Duration) error {
	err := a.Store.updateState(func(s *state) {
		if d <= 0 {
			s.PausedUntil = 0
		} else {
			s.PausedUntil = a.now().Add(d).Unix()
		}
	})
	if err == nil {
		a.Kick()
		if a.OnChange != nil {
			a.OnChange()
		}
	}
	return err
}

// RequestReview просит ревью MR по ссылке, введённой человеком.
func (a *Agent) RequestReview(ctx context.Context, link string) (mrurl.MR, Requested, error) {
	mr, err := mrurl.NormalizeMR(link)
	if err != nil {
		return mrurl.MR{}, Requested{}, err
	}
	c := a.Store.Get()
	if c.Key == "" {
		return mr, Requested{}, errors.New("клиент не подключён к брокеру")
	}
	if c.ProjectFor(mr.Repo.String()) == nil {
		return mr, Requested{}, fmt.Errorf("проекта %s нет в настройках: добавь его в projects", mr.Repo)
	}
	r, err := a.api().Request(ctx, mr.String())
	if err == nil {
		a.set(func(s *State) { s.Balance = r.Balance })
	}
	return mr, r, err
}

// DescribeRequest — текст уведомления о принятом запросе.
func DescribeRequest(mr mrurl.MR, r Requested) string {
	if r.Waiting {
		return fmt.Sprintf("%s: свободных ревьюеров нет, запрос ждёт до 24 ч. Баланс %d", mr.Short(), r.Balance)
	}
	return fmt.Sprintf("%s: запрос ушёл %s — уведомлю, кто возьмёт. Баланс %d", mr.Short(), strings.Join(r.Reviewers, ", "), r.Balance)
}
