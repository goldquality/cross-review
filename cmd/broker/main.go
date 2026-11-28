// broker — сервер cross-review: раздаёт ссылки на MR участникам и считает баллы.
//
//	broker [serve]                   запустить сервер (по умолчанию)
//	broker invite <имя>              личное приглашение: одноразовое, на 7 дней
//	broker invites                   неиспользованные приглашения
//	broker members                   участники, баланс, активность
//	broker revoke <имя>              отозвать ключ участника; имя остаётся за ним
//	broker grant <имя> <n>           начислить (или списать, n < 0) баллы
//	broker repos [add|remove <url>]  разрешённые репо
//	broker snapshot                  записать снимок базы сейчас
//
// Переменные:
//   - BROKER_ADDR (:8080), BROKER_DB (/data/broker.db);
//   - BROKER_PUBLIC_URL — адрес для клиентов, печатается рядом с приглашением;
//   - BROKER_TRUST_PROXY=1 — брокер за прокси в частной сети Docker, или BROKER_TRUSTED_PROXIES —
//     подсети прокси через запятую: только им брокер верит X-Forwarded-For;
//   - BROKER_SNAPSHOT (/data/snapshot.db) — целостная копия базы для бэкапа раз в 15 минут, «-» —
//     не писать.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/goldquality/cross-review/internal/broker"
)

// version — тег релиза, задаётся при сборке образа (-ldflags -X main.version).
var version = "dev"

const snapshotEvery = 15 * time.Minute

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func run(args []string) error {
	db, err := broker.OpenDB(env("BROKER_DB", "/data/broker.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	s := broker.NewService(db)
	s.Version = version
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		return serve(s)
	case "invite":
		if len(args) != 1 {
			return errors.New("broker invite <имя>")
		}
		inv, err := s.CreateInvite(args[0])
		if err != nil {
			return err
		}
		printInvite(inv)
		return nil
	case "invites":
		list, err := s.Invites()
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Println("Неиспользованных приглашений нет.")
		}
		for _, p := range list {
			fmt.Printf("%s — до %s\n", p.Name, time.Unix(p.ExpiresAt, 0).Format("02.01 15:04"))
		}
		return nil
	case "members":
		return members(s)
	case "revoke":
		if len(args) != 1 {
			return errors.New("broker revoke <имя>")
		}
		if err := s.Revoke(args[0]); err != nil {
			return err
		}
		fmt.Println("ключ отозван:", args[0])
		return nil
	case "grant":
		if len(args) != 2 {
			return errors.New("broker grant <имя> <n>")
		}
		n, err := strconv.Atoi(args[1])
		if err != nil {
			return errors.New("n — целое число")
		}
		if err := s.Grant(args[0], n); err != nil {
			return err
		}
		fmt.Printf("%s: %+d\n", args[0], n)
		return nil
	case "repos":
		return repos(s, args)
	case "snapshot":
		path := snapshotPath()
		if path == "" {
			return errors.New("снимки выключены: BROKER_SNAPSHOT=-")
		}
		if err := s.Snapshot(path); err != nil {
			return err
		}
		fmt.Println("снимок базы:", path)
		return nil
	}
	return fmt.Errorf("неизвестная команда %q: serve, invite, invites, members, revoke, grant, repos, snapshot", cmd)
}

func printInvite(inv broker.Invite) {
	fmt.Printf("Приглашение для %s: %s\n", inv.Name, inv.Code)
	fmt.Printf("Одноразовое, действует до %s. Показывается один раз.\n", time.Unix(inv.ExpiresAt, 0).Format("02.01 15:04"))
	if inv.Rekey {
		fmt.Println("Участник уже есть: вход по приглашению выдаст ему новый ключ, прежний перестанет работать, баллы останутся.")
	}
	if u := os.Getenv("BROKER_PUBLIC_URL"); u != "" {
		fmt.Println("Адрес брокера:", u)
	}
}

func repos(s *broker.Service, args []string) error {
	if len(args) == 0 {
		list, err := s.Repos()
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Println("Разрешённых репо нет: broker repos add <адрес репо>")
		}
		for _, r := range list {
			fmt.Println(r)
		}
		return nil
	}
	if len(args) != 2 || (args[0] != "add" && args[0] != "remove") {
		return errors.New("broker repos [add|remove <адрес репо>]")
	}
	if args[0] == "add" {
		r, err := s.AddRepo(args[1])
		if err != nil {
			return err
		}
		fmt.Println("разрешён:", r)
		return nil
	}
	r, err := s.RemoveRepo(args[1])
	if err != nil {
		return err
	}
	fmt.Println("запрещён:", r)
	return nil
}

func snapshotPath() string {
	p := env("BROKER_SNAPSHOT", "/data/snapshot.db")
	if p == "-" {
		return ""
	}
	return p
}

// proxies — от каких адресов брокер верит X-Forwarded-For.
func proxies() ([]netip.Prefix, error) {
	if v := os.Getenv("BROKER_TRUSTED_PROXIES"); v != "" {
		return broker.ParseProxies(v)
	}
	if os.Getenv("BROKER_TRUST_PROXY") == "1" {
		return broker.PrivateNets, nil
	}
	return nil, nil
}

func serve(s *broker.Service) error {
	logger := log.New(os.Stderr, "", log.LstdFlags)
	trusted, err := proxies()
	if err != nil {
		return err
	}
	if list, err := s.Repos(); err == nil && len(list) == 0 {
		logger.Printf("разрешённых репо нет: broker repos add <адрес репо>")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go s.RunTicker(ctx, 15*time.Second, logger.Printf)
	if path := snapshotPath(); path != "" {
		go func() {
			t := time.NewTicker(snapshotEvery)
			defer t.Stop()
			for {
				if err := s.Snapshot(path); err != nil {
					logger.Printf("снимок базы: %v", err)
				}
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
		}()
	}
	srv := &http.Server{
		Addr:              env("BROKER_ADDR", ":8080"),
		Handler:           s.Handler(logger, trusted),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      90 * time.Second,
		IdleTimeout:       120 * time.Second,
		// Запросы живут в ctx сервера: по SIGTERM долгие опросы сразу отвечают «заданий нет», и
		// остановка не ждёт их 30 секунд.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	logger.Printf("брокер %s слушает %s", version, srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-stopped // база закрывается после того, как ответили последние запросы
	return nil
}

func members(s *broker.Service) error {
	list, err := s.Members()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ИМЯ\tБАЛЛЫ\tРЕВЬЮ\tПРОЕКТОВ\tБЫЛ\tПАУЗА ДО\tВЕРСИЯ")
	now := time.Now().Unix()
	for _, m := range list {
		seen := "никогда"
		if m.LastSeen > 0 {
			seen = (time.Duration(now-m.LastSeen) * time.Second).String() + " назад"
		}
		pause := ""
		if m.PausedUntil > now {
			pause = time.Unix(m.PausedUntil, 0).Format("02.01 15:04")
		}
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%s\t%s\t%s\n", m.Name, m.Balance, m.Done, m.Repos, seen, pause, strings.TrimSpace(m.Version))
	}
	return w.Flush()
}
