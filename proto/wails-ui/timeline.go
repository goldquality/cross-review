package main

import (
	"fmt"
	"sync"
	"time"
)

// timeline — что и когда происходило: тост показан, сдвинут, закрыт; на Windows — чьё окно стало
// активным. По ней видно, какое действие отдаёт фокус тосту.
var timeline = struct {
	sync.Mutex
	start time.Time
	lines []string
}{start: time.Now()}

func mark(format string, args ...any) {
	timeline.Lock()
	defer timeline.Unlock()
	timeline.lines = append(timeline.lines, fmt.Sprintf("%5.2fs ", time.Since(timeline.start).Seconds())+fmt.Sprintf(format, args...))
}

func timelineLines() []string {
	timeline.Lock()
	defer timeline.Unlock()
	return append([]string(nil), timeline.lines...)
}

// watchForeground записывает в ленту каждую смену активного окна (только Windows).
func watchForeground() {
	if _, _, ok := foreground(); !ok {
		return
	}
	last := ""
	for range time.Tick(250 * time.Millisecond) {
		title, ours, _ := foreground()
		cur := fmt.Sprintf("активно «%s», наше: %v", title, ours)
		if cur != last {
			last = cur
			mark("%s", cur)
		}
	}
}
