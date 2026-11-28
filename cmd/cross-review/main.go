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
	"sync"
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
  cross-review config                  где файл настроек и что в нём не так; нет файла — создать шаблон
  cross-review projects                проекты из настроек
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

func run(args []string) error {
	dir, err := client.Dir()
	if err != nil {
		return err
	}
	settings, err := client.SettingsPath()
	if err != nil {
		return err
	}
	store, err := client.OpenStore(dir, settings)
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
	case "config":
		return config(store)
	case "projects":
		return projects(store)
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
	fmt.Printf("%s  %s\n", time.Now().Format("15:04:05"), client.Clean(text))
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
	if c.Err != "" {
		return fmt.Errorf("%s: %s", c.Path, c.Err)
	}
	who := ""
	if c.Name != "" {
		who = c.Name + ", "
	}
	fmt.Printf("cross-review %s: %sброкер %s, проектов %d. Ctrl-C — выход.\n", version, who, c.Broker, len(c.Projects))
	a := newAgent(dir, store, printNotifier{})
	// Трея нет: ошибки связи и входа печатаем, когда они меняются, иначе daemon молчал бы о них.
	var mu sync.Mutex
	lastErr := ""
	a.OnChange = func() {
		e := a.State().LastError
		mu.Lock()
		defer mu.Unlock()
		if e != lastErr {
			lastErr = e
			if e != "" {
				printNotifier{}.Notify("", "Ошибка: "+e)
			}
		}
	}
	a.Run(ctx)
	return nil
}

func config(store *client.Store) error {
	created, err := store.EnsureSettings()
	if err != nil {
		return err
	}
	fmt.Println("Настройки:", store.Path())
	if created {
		fmt.Println("Файла не было — создал шаблон. Впиши в него broker, invite, name и проекты.")
		return nil
	}
	c := store.Get()
	if c.Err != "" {
		return errors.New(c.Err)
	}
	name := c.Name
	if name == "" {
		name = "задаст приглашение"
	}
	fmt.Printf("В порядке: брокер %s, имя %s, проектов %d.\n", c.Broker, name, len(c.Projects))
	if c.Key == "" {
		fmt.Println("К брокеру ещё не подключён: трей или daemon подключится сам по invite.")
	}
	return nil
}

func projects(store *client.Store) error {
	c := store.Get()
	if c.Err != "" {
		fmt.Fprintf(os.Stderr, "В настройках ошибка, показаны последние исправные: %s\n", c.Err)
	}
	if len(c.Projects) == 0 {
		fmt.Println("Проектов нет: впиши их в", c.Path)
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "РЕПО\tПАПКА ЗАПУСКА\tКОМАНДА")
	for _, p := range c.Projects {
		fmt.Fprintf(w, "%s\t%s\t%s\n", p.Repo, p.Workdir, client.FormatCommand(p.Command))
	}
	return w.Flush()
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
		// Строки брокера — через Clean: в терминал не должны попасть управляющие последовательности.
		fmt.Printf("%s  %s  %s\n    %s\n", time.Unix(r.CreatedAt, 0).Format("02.01 15:04"), requestStatus(r.Status), client.Clean(r.MRURL), client.Clean(strings.Join(jobs, "; ")))
	}
	return nil
}

func jobStatus(s string) string {
	return map[string]string{"pending": "ждёт", "assigned": "назначено", "running": "ревьюит", "done": "готово", "failed": "не вышло", "declined": "отказался", "expired": "никто не взял"}[s]
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
	fmt.Println("Настройки:", c.Path)
	if c.Err != "" {
		fmt.Println("Ошибка в настройках:", c.Err)
	}
	if c.Key == "" {
		fmt.Println("К брокеру ещё не подключён: трей или daemon подключится сам по invite из настроек.")
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
	c := store.Get()
	if c.Err != "" {
		return fmt.Errorf("%s: %s", c.Path, c.Err)
	}
	p := c.ProjectFor(mr.Repo.String())
	if p == nil {
		return fmt.Errorf("проекта %s нет в настройках", mr.Repo)
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
