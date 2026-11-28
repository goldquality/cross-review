package main

/*
#include <stdlib.h>

// crToastShow показывает тост. buttons — подписи через \n, пусто — без кнопок; primary — номер
// главной кнопки или -1. Без кнопок тост закрывается сам через seconds или по клику; с кнопками —
// только по crToastClose, а seconds — длина полоски обратного отсчёта (0 — без неё). Клик по кнопке
// и закрытие тоста без кнопок приходят в Go через crToastDone.
void crToastShow(long long id, const char *title, const char *body, const char *footer,
                 const char *buttons, int primary, double seconds, int sound,
                 const void *icon, int iconLen);

void crToastClose(long long id);
*/
import "C"

import (
	"context"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/goldquality/cross-review/internal/client"
)

// toastSpec — что показать в тосте. Текст от брокера к этому моменту уже прошёл client.Clean.
type toastSpec struct {
	title, body, footer string
	buttons             []string
	primary             int           // номер главной кнопки, -1 — нет
	life                time.Duration // без кнопок — сколько висеть; с кнопками — полоска обратного отсчёта
	sound               bool
}

// toastClosed — тост без кнопок закрылся: сам или по клику.
const toastClosed = -1

var toasts = struct {
	sync.Mutex
	next int64
	wait map[int64]chan int
}{wait: map[int64]chan int{}}

var toastIcon = sync.OnceValue(func() []byte { return encodePNG(drawIcon(inkBlue, stateOnline)) })

// showToast показывает тост; в канал приходят номера нажатых кнопок.
func showToast(s toastSpec) (int64, <-chan int) {
	toasts.Lock()
	toasts.next++
	id := toasts.next
	ch := make(chan int, 8)
	toasts.wait[id] = ch
	toasts.Unlock()
	strs := []*C.char{C.CString(s.title), C.CString(s.body), C.CString(s.footer), C.CString(strings.Join(s.buttons, "\n"))}
	defer func() {
		for _, p := range strs {
			C.free(unsafe.Pointer(p))
		}
	}()
	sound := 0
	if s.sound {
		sound = 1
	}
	icon := toastIcon()
	C.crToastShow(C.longlong(id), strs[0], strs[1], strs[2], strs[3], C.int(s.primary), C.double(s.life.Seconds()),
		C.int(sound), unsafe.Pointer(&icon[0]), C.int(len(icon)))
	return id, ch
}

func closeToast(id int64) {
	toasts.Lock()
	delete(toasts.wait, id)
	toasts.Unlock()
	C.crToastClose(C.longlong(id))
}

// crToastDone зовётся из главного потока Cocoa: ждать в нём нельзя, поэтому отправка без блокировки.
//
//export crToastDone
func crToastDone(id C.longlong, action C.int) {
	toasts.Lock()
	ch := toasts.wait[int64(id)]
	if action == toastClosed {
		delete(toasts.wait, int64(id))
	}
	toasts.Unlock()
	if ch != nil {
		select {
		case ch <- int(action):
		default:
		}
	}
}

// showNotification — тост справа вверху, закрывается сам через 6 секунд.
func showNotification(title, text string) {
	showToast(toastSpec{title: title, body: text, primary: -1, life: 6 * time.Second})
}

// askOffer — тост с кнопками «Открыть MR», «Отклонить», «Принять» и полоской обратного отсчёта.
// «Открыть MR» открывает ссылку, тост остаётся. Истёк ctx — тост закрывается, ответ «нет».
func askOffer(ctx context.Context, o client.JobOffer) bool {
	const open, decline, accept = 0, 1, 2
	l := offerText(o)
	id, ch := showToast(toastSpec{
		title: l.who, body: l.run, footer: l.until,
		buttons: []string{"Открыть MR", "Отклонить", "Принять"}, primary: accept,
		life: time.Until(o.Until), sound: true,
	})
	defer closeToast(id)
	for {
		select {
		case <-ctx.Done():
			return false
		case a := <-ch:
			switch a {
			case open:
				openURL(o.MR.String())
			case accept:
				return true
			default:
				return false
			}
		}
	}
}
