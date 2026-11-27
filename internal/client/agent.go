package client

import (
	"context"
	"errors"
	"fmt"
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
	Configured  bool
	Name        string
	Online      bool
	Balance     int
	PausedUntil time.Time
	Current     *Running
	LastError   string
	LastLog     string
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

	mu      sync.Mutex
	state   State
	cancel  context.CancelFunc
	kick    chan struct{}
	started bool
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

// Kick — отметиться сейчас, не дожидаясь 30 секунд: после паузы, правки проектов, входа.
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
	if c.Broker == "" || c.Key == "" {
		return
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
			msg = "ключ отозван или не подходит: войди заново"
		}
		a.set(func(s *State) { s.Online = false; s.LastError = msg })
		return
	}
	a.set(func(s *State) { s.Online = true; s.Balance = b.Balance; s.LastError = "" })
	for _, e := range b.Events {
		a.Notify.Notify("cross-review", e.Text)
	}
}

func (a *Agent) jobLoop(ctx context.Context) {
	for ctx.Err() == nil {
		c := a.Store.Get()
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
		rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := api.Result(rctx, job.ID, r); err != nil {
			a.set(func(s *State) { s.LastError = "итог не отправлен: " + err.Error() })
		}
		a.Kick()
	}
	mr, err := mrurl.ParseMR(job.MRURL)
	if err != nil {
		report(Result{ExitCode: -2, Tail: "клиент отклонил ссылку: " + err.Error()})
		a.Notify.Notify("cross-review", "Брокер прислал негодную ссылку, задание отклонено")
		return
	}
	c := a.Store.Get()
	p := c.ProjectFor(mr.Repo.String())
	if p == nil {
		report(Result{ExitCode: -2, Tail: "проект " + mr.Repo.String() + " не подключён у ревьюера"})
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
	err := a.Store.Update(func(c *Config) error {
		if d <= 0 {
			c.PausedUntil = 0
		} else {
			c.PausedUntil = a.now().Add(d).Unix()
		}
		return nil
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
		return mr, Requested{}, fmt.Errorf("проект %s не подключён: добавь его в меню «Проекты»", mr.Repo)
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
	return fmt.Sprintf("%s: ревьюят %s. Баланс %d", mr.Short(), strings.Join(r.Reviewers, ", "), r.Balance)
}
