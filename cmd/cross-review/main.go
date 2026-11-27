// cross-review — клиент кросс-ревью: живёт в трее, берёт задания на ревью чужих MR и просит
// ревью своих.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/gofrs/flock"

	"github.com/goldquality/cross-review/internal/client"
	"github.com/goldquality/cross-review/internal/mrurl"
)

var version = "dev"

const usage = `cross-review — кросс-ревью MR силами команды.

  cross-review                         трей (так он запускается при входе в систему)
  cross-review join <адрес брокера> <приглашение> --name Имя [--gitlab вход]
  cross-review projects                подключённые проекты
  cross-review projects scan <адрес репо>
  cross-review projects add <адрес репо> --clone ПУТЬ [--workdir ПУТЬ] --command "./dev review <LINK>"
  cross-review projects remove <адрес репо>
  cross-review request <ссылка на MR>  попросить ревью (−1 балл)
  cross-review requests                мои запросы
  cross-review pause <1|3|6|12|24|off> пауза в часах
  cross-review status
  cross-review run <ссылка на MR>      прогнать свою команду ревью локально, без брокера
  cross-review daemon                  без трея: задания и уведомления в консоль
  cross-review install | uninstall     автозапуск при входе в систему
  cross-review version`

func main() {
	attachConsole()
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
}

// opts — позиционные аргументы и --флаги в любом порядке.
type opts struct {
	pos   []string
	flags map[string]string
}

func parse(args []string, known ...string) (opts, error) {
	o := opts{flags: map[string]string{}}
	isKnown := map[string]bool{}
	for _, k := range known {
		isKnown[k] = true
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			o.pos = append(o.pos, a)
			continue
		}
		name, val, hasVal := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		if !isKnown[name] {
			return o, fmt.Errorf("неизвестный флаг --%s", name)
		}
		if !hasVal {
			if i+1 >= len(args) {
				return o, fmt.Errorf("--%s без значения", name)
			}
			i++
			val = args[i]
		}
		o.flags[name] = val
	}
	return o, nil
}

func run(args []string) error {
	dir, err := client.Dir()
	if err != nil {
		return err
	}
	store, err := client.OpenStore(dir)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return runTray(dir, store)
	}
	cmd, rest := args[0], args[1:]
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch cmd {
	case "tray":
		return runTray(dir, store)
	case "daemon":
		return daemon(ctx, dir, store)
	case "join":
		return join(ctx, store, rest)
	case "projects":
		return projects(ctx, store, rest)
	case "request":
		return request(ctx, store, rest)
	case "requests":
		return requests(ctx, store)
	case "pause":
		return pause(store, rest)
	case "status":
		return status(ctx, store)
	case "run":
		return runOnce(ctx, dir, store, rest)
	case "install":
		return install()
	case "uninstall":
		return uninstall()
	case "version":
		fmt.Println(version)
		return nil
	case "help", "-h", "--help":
		fmt.Println(usage)
		return nil
	}
	return fmt.Errorf("неизвестная команда %q\n\n%s", cmd, usage)
}

// lockInstance — один трей или daemon на папку клиента.
func lockInstance(dir string) (*flock.Flock, error) {
	l := flock.New(filepath.Join(dir, "instance.lock"))
	ok, err := l.TryLock()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("cross-review уже запущен")
	}
	return l, nil
}

type printNotifier struct{}

func (printNotifier) Notify(title, text string) {
	fmt.Printf("%s  %s\n", time.Now().Format("15:04:05"), text)
}

// newAgent — агент клиента. PATH логин-шелла нужен только тому, кто запускает команды ревью.
func newAgent(dir string, store *client.Store, n client.Notifier) *client.Agent {
	return &client.Agent{
		Store:     store,
		Notify:    n,
		LogDir:    filepath.Join(dir, "logs"),
		Version:   version,
		ShellPath: client.LoginShellPath(),
	}
}

func daemon(ctx context.Context, dir string, store *client.Store) error {
	l, err := lockInstance(dir)
	if err != nil {
		return err
	}
	defer l.Unlock()
	c := store.Get()
	if c.Key == "" {
		return errors.New("клиент не подключён: cross-review join …")
	}
	fmt.Printf("cross-review %s: %s, брокер %s, проектов %d. Ctrl-C — выход.\n", version, c.Name, c.Broker, len(c.Projects))
	newAgent(dir, store, printNotifier{}).Run(ctx)
	return nil
}

func join(ctx context.Context, store *client.Store, args []string) error {
	o, err := parse(args, "name", "gitlab")
	if err != nil {
		return err
	}
	if len(o.pos) != 2 || o.flags["name"] == "" {
		return errors.New("cross-review join <адрес брокера> <приглашение> --name Имя [--gitlab вход]")
	}
	return doJoin(ctx, store, o.pos[0], o.pos[1], o.flags["name"], o.flags["gitlab"], func(s string) { fmt.Println(s) })
}

func doJoin(ctx context.Context, store *client.Store, brokerURL, invite, name, gitlabUser string, say func(string)) error {
	base, err := client.CheckBrokerURL(brokerURL)
	if err != nil {
		return err
	}
	j, err := client.NewAPI(base, "").Join(ctx, strings.TrimSpace(invite), name, gitlabUser)
	if err != nil {
		return err
	}
	if err := store.Update(func(c *client.Config) error {
		c.Broker, c.Key, c.Name = base, j.Key, j.Name
		return nil
	}); err != nil {
		return err
	}
	say(fmt.Sprintf("Подключено как %s, баланс %d. Ключ сохранён, приглашение больше не нужно.", j.Name, j.Balance))
	return nil
}

func projects(ctx context.Context, store *client.Store, args []string) error {
	if len(args) == 0 {
		c := store.Get()
		if len(c.Projects) == 0 {
			fmt.Println("Проектов нет: cross-review projects add …")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "РЕПО\tКЛОН\tПАПКА ЗАПУСКА\tКОМАНДА")
		for _, p := range c.Projects {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", p.Repo, p.Clone, p.Workdir, client.FormatCommand(p.Command))
		}
		return w.Flush()
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "scan":
		if len(rest) != 1 {
			return errors.New("cross-review projects scan <адрес репо>")
		}
		repo, err := mrurl.ParseRemote(rest[0])
		if err != nil {
			return err
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		fmt.Printf("Ищу клоны %s в %s (глубина %d)…\n", repo, home, client.ScanDepth)
		n := 0
		sctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		err = client.Scan(sctx, home, client.ScanDepth, repo, func(p string) { n++; fmt.Println(" ", p) })
		if n == 0 {
			fmt.Println("Не нашёл: укажи путь в --clone вручную.")
		}
		return err
	case "add":
		o, err := parse(rest, "clone", "workdir", "command")
		if err != nil {
			return err
		}
		if len(o.pos) != 1 || o.flags["clone"] == "" || o.flags["command"] == "" {
			return errors.New(`cross-review projects add <адрес репо> --clone ПУТЬ [--workdir ПУТЬ] --command "./dev review <LINK>"`)
		}
		p, err := client.NewProject(o.pos[0], o.flags["clone"], o.flags["workdir"], o.flags["command"])
		if err != nil {
			return err
		}
		if err := addProject(store, p); err != nil {
			return err
		}
		fmt.Printf("Проект %s подключён: %s в %s\n", p.Repo, client.FormatCommand(p.Command), p.Workdir)
		return nil
	case "remove":
		if len(rest) != 1 {
			return errors.New("cross-review projects remove <адрес репо>")
		}
		repo, err := mrurl.ParseRemote(rest[0])
		if err != nil {
			return err
		}
		return removeProject(store, repo.String())
	}
	return fmt.Errorf("projects: scan, add, remove")
}

func addProject(store *client.Store, p client.Project) error {
	return store.Update(func(c *client.Config) error {
		for i := range c.Projects {
			if c.Projects[i].Repo == p.Repo {
				c.Projects[i] = p
				return nil
			}
		}
		c.Projects = append(c.Projects, p)
		return nil
	})
}

func removeProject(store *client.Store, repo string) error {
	return store.Update(func(c *client.Config) error {
		for i := range c.Projects {
			if c.Projects[i].Repo == repo {
				c.Projects = append(c.Projects[:i], c.Projects[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("проект %s не подключён", repo)
	})
}

func request(ctx context.Context, store *client.Store, args []string) error {
	if len(args) != 1 {
		return errors.New("cross-review request <ссылка на MR>")
	}
	a := &client.Agent{Store: store, Notify: printNotifier{}, Version: version}
	mr, r, err := a.RequestReview(ctx, args[0])
	if err != nil {
		return err
	}
	fmt.Println(client.DescribeRequest(mr, r))
	return nil
}

func requests(ctx context.Context, store *client.Store) error {
	c := store.Get()
	list, err := client.NewAPI(c.Broker, c.Key).MyRequests(ctx)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("Запросов не было.")
		return nil
	}
	for _, r := range list {
		var jobs []string
		for _, j := range r.Jobs {
			who := j.Reviewer
			if who == "" {
				who = "ждёт ревьюера"
			}
			jobs = append(jobs, who+": "+jobStatus(j.Status))
		}
		fmt.Printf("%s  %s  %s\n    %s\n", time.Unix(r.CreatedAt, 0).Format("02.01 15:04"), requestStatus(r.Status), r.MRURL, strings.Join(jobs, "; "))
	}
	return nil
}

func jobStatus(s string) string {
	return map[string]string{"pending": "ждёт", "assigned": "назначено", "running": "ревьюит", "done": "готово", "failed": "не вышло", "expired": "никто не взял"}[s]
}

func requestStatus(s string) string {
	return map[string]string{"open": "идёт", "done": "закрыт"}[s]
}

// PauseHours — варианты паузы в меню и в CLI.
var PauseHours = []int{1, 3, 6, 12, 24}

func pause(store *client.Store, args []string) error {
	if len(args) != 1 {
		return errors.New("cross-review pause <1|3|6|12|24|off>")
	}
	a := &client.Agent{Store: store}
	if args[0] == "off" {
		if err := a.SetPause(0); err != nil {
			return err
		}
		fmt.Println("Пауза снята.")
		return nil
	}
	h, err := strconv.Atoi(args[0])
	ok := false
	for _, v := range PauseHours {
		ok = ok || v == h
	}
	if err != nil || !ok {
		return errors.New("пауза: 1, 3, 6, 12 или 24 часа, off — снять")
	}
	if err := a.SetPause(time.Duration(h) * time.Hour); err != nil {
		return err
	}
	fmt.Printf("Пауза до %s: задания не приходят. Трей подхватит её в течение 30 секунд.\n", time.Now().Add(time.Duration(h)*time.Hour).Format("02.01 15:04"))
	return nil
}

func status(ctx context.Context, store *client.Store) error {
	c := store.Get()
	if c.Key == "" {
		fmt.Println("Не подключён: cross-review join <адрес брокера> <приглашение> --name Имя")
		return nil
	}
	fmt.Printf("Брокер: %s\nИмя: %s\nПроектов: %d\n", c.Broker, c.Name, len(c.Projects))
	if c.Paused(time.Now()) {
		fmt.Println("Пауза до", time.Unix(c.PausedUntil, 0).Format("02.01 15:04"))
	}
	me, err := client.NewAPI(c.Broker, c.Key).Whoami(ctx)
	if err != nil {
		return err
	}
	online := "нет"
	if time.Now().Unix()-me.LastSeen < 90 {
		online = "да"
	}
	fmt.Printf("Баланс: %d\nОнлайн у брокера: %s\n", me.Balance, online)
	return nil
}

func runOnce(ctx context.Context, dir string, store *client.Store, args []string) error {
	if len(args) != 1 {
		return errors.New("cross-review run <ссылка на MR>")
	}
	mr, err := mrurl.NormalizeMR(args[0])
	if err != nil {
		return err
	}
	p := store.Get().ProjectFor(mr.Repo.String())
	if p == nil {
		return fmt.Errorf("проект %s не подключён", mr.Repo)
	}
	fmt.Printf("$ %s  (в %s)\n", client.FormatCommand(client.Substitute(p.Command, mr.String())), p.Workdir)
	res, err := client.Run(ctx, *p, mr, filepath.Join(dir, "logs"), client.ChildEnv(client.LoginShellPath()))
	if err != nil {
		return err
	}
	fmt.Printf("Код выхода %d за %s. Лог: %s\n", res.ExitCode, res.Duration.Round(time.Second), res.LogPath)
	if res.ExitCode != 0 {
		os.Exit(res.ExitCode)
	}
	return nil
}
