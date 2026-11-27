// broker — сервер cross-review: раздаёт ссылки на MR участникам и считает баллы.
//
//	broker [serve]          запустить сервер (по умолчанию)
//	broker invite [--rotate] показать или заменить приглашение
//	broker members          участники, баланс, активность
//	broker revoke <имя>     отозвать ключ участника
//	broker grant <имя> <n>  начислить (или списать, n < 0) баллы
//
// Переменные: BROKER_ADDR (:8080), BROKER_DB (/data/broker.db), BROKER_PUBLIC_URL — адрес для
// клиентов, печатается рядом с приглашением; BROKER_TRUST_PROXY=1 — брокер за Caddy.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/goldquality/cross-review/internal/broker"
)

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
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		return serve(s)
	case "invite":
		var inv string
		if len(args) == 1 && args[0] == "--rotate" {
			inv, err = s.RotateInvite()
		} else {
			inv, _, err = s.EnsureInvite()
		}
		if err != nil {
			return err
		}
		printInvite(inv)
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
	}
	return fmt.Errorf("неизвестная команда %q: serve, invite, members, revoke, grant", cmd)
}

func printInvite(inv string) {
	fmt.Println("Приглашение в cross-review:", inv)
	if u := os.Getenv("BROKER_PUBLIC_URL"); u != "" {
		fmt.Println("Адрес брокера:", u)
	}
	fmt.Println("Участник вводит адрес и приглашение в клиенте; приглашение меняется на личный ключ.")
}

func serve(s *broker.Service) error {
	logger := log.New(os.Stderr, "", log.LstdFlags)
	inv, created, err := s.EnsureInvite()
	if err != nil {
		return err
	}
	if created {
		printInvite(inv)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go s.RunTicker(ctx, 15*time.Second, logger.Printf)
	srv := &http.Server{
		Addr:              env("BROKER_ADDR", ":8080"),
		Handler:           s.Handler(logger, os.Getenv("BROKER_TRUST_PROXY") == "1"),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      90 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	logger.Printf("брокер слушает %s", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func members(s *broker.Service) error {
	list, err := s.Members()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ИМЯ\tGITLAB\tБАЛЛЫ\tРЕВЬЮ\tПРОЕКТОВ\tБЫЛ\tПАУЗА ДО\tВЕРСИЯ")
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
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%s\t%s\t%s\n", m.Name, m.GitlabUser, m.Balance, m.Done, m.Repos, seen, pause, strings.TrimSpace(m.Version))
	}
	return w.Flush()
}
