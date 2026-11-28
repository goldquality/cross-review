package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/goldquality/cross-review/internal/client"
)

// UI — методы, которые зовут страницы окна и панели тостов. Каждый экспортированный метод доступен
// JS, поэтому здесь только намерения: прочитать состояние, попросить ревью по ссылке, поставить
// паузу, ответить на тост. Ни команды, ни пути, ни адреса для открытия страница не передаёт: лог,
// файл настроек, ссылку на MR и страницу релизов Go берёт сам. Ссылку на MR из поля ввода
// разбирает Agent.RequestReview так же строго, как ссылку от брокера. Список методов закреплён
// тестом TestUIMethods.
type UI struct{ d *desktop }

// View — состояние для окна у трея.
func (u *UI) View() View {
	return buildView(u.d.agent.State(), u.d.store.Get().Projects, autostartInstalled())
}

// Requests — «Мои запросы» с брокера.
func (u *UI) Requests() ([]RequestView, error) {
	c := u.d.store.Get()
	if c.Key == "" {
		return nil, errors.New("клиент ещё не подключён к брокеру")
	}
	ctx, cancel := context.WithTimeout(u.d.ctx, 30*time.Second)
	defer cancel()
	list, err := client.NewAPI(c.Broker, c.Key).MyRequests(ctx)
	if err != nil {
		return nil, errors.New(client.Clean(err.Error()))
	}
	return requestViews(list), nil
}

// ClipboardMR — ссылка на MR из буфера обмена, чтобы подставить её в поле; пусто — ссылки нет.
func (u *UI) ClipboardMR() string { return u.d.clipboardMR() }

// Request просит ревью MR по ссылке из поля ввода.
func (u *UI) Request(link string) (string, error) {
	msg, err := u.d.request(link)
	if err != nil {
		return "", errors.New(client.Clean(err.Error()))
	}
	return client.Clean(msg), nil
}

// Pause ставит паузу на hours часов из PauseHours; 0 снимает её.
func (u *UI) Pause(hours int) error {
	if hours != 0 && !slices.Contains(PauseHours, hours) {
		return fmt.Errorf("пауза: 1, 3, 6, 12 или 24 часа")
	}
	return u.d.agent.SetPause(time.Duration(hours) * time.Hour)
}

// StopCurrent останавливает текущее ревью; оно уйдёт другому участнику.
func (u *UI) StopCurrent() {
	if u.d.agent.StopCurrent() {
		u.d.Notify(appTitle, "Ревью остановлено, задание уйдёт другому")
	}
}

// OpenLastLog открывает лог последнего ревью.
func (u *UI) OpenLastLog() {
	if p := u.d.agent.State().LastLog; p != "" {
		openFile(p)
	}
}

// OpenSettings открывает файл настроек в редакторе; файла нет — создаёт шаблон.
func (u *UI) OpenSettings() error {
	if _, err := u.d.store.EnsureSettings(); err != nil {
		return err
	}
	openEditor(u.d.store.Path())
	return nil
}

// OpenReleases открывает страницу релизов.
func (u *UI) OpenReleases() { openURL(client.ReleasesURL) }

// SetAutostart включает или выключает запуск при входе в систему.
func (u *UI) SetAutostart(on bool) error {
	if on {
		return install()
	}
	return uninstall()
}

// HideWindow прячет окно у трея.
func (u *UI) HideWindow() { u.d.popover.Hide() }

// Quit — выход из клиента; текущее ревью останавливается.
func (u *UI) Quit() { u.d.quit() }

// Toasts — тосты для панели.
func (u *UI) Toasts() []ToastView { return u.d.toasts.views() }

// ToastAction — нажата кнопка action тоста id; -1 — клик по тосту без кнопок.
func (u *UI) ToastAction(id int64, action int) { u.d.toasts.action(id, action) }

// ToastHover — мышь над тостом: тост без кнопок не закрывается, пока его читают.
func (u *UI) ToastHover(id int64, on bool) { u.d.toasts.hover(id, on) }

// PanelResize — панель тостов измерила свою высоту.
func (u *UI) PanelResize(height int) { u.d.toasts.resize(height) }
