//go:build !darwin

package main

import (
	"context"
	"errors"

	"github.com/ncruces/zenity"

	"github.com/goldquality/cross-review/internal/client"
)

// showNotification — системное уведомление: на Windows тост через PowerShell, на Linux — через
// zenity или kdialog.
func showNotification(title, text string) { zenity.Notify(text, zenity.Title(title)) }

// askOffer — окно «Принять / Отклонить» поверх остальных, уведомление зовёт к нему. Окно
// закрывается само, когда ctx истекает.
func askOffer(ctx context.Context, o client.JobOffer) bool {
	l := offerText(o)
	trayNotifier{}.Notify(appTitle, l.who)
	text := l.who + "\n" + o.MR.String() + "\n\n" + l.run + ".\n" + l.until + "."
	for {
		err := zenity.Question(text, zenity.Title(l.title), zenity.OKLabel("Принять"),
			zenity.CancelLabel("Отклонить"), zenity.ExtraButton("Открыть MR"), zenity.Context(ctx))
		switch {
		case err == nil:
			return true
		case errors.Is(err, zenity.ErrExtraButton):
			openURL(o.MR.String()) // и спросить снова
		case errors.Is(err, zenity.ErrCanceled) || ctx.Err() != nil:
			return false
		default:
			trayNotifier{}.Notify(appTitle, "Окно с вопросом не открылось, задание ушло другому: "+err.Error())
			return false
		}
	}
}
