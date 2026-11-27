package client

import (
	"strconv"
	"strings"
)

// ReleasesURL — где скачать новую версию клиента.
const ReleasesURL = "https://github.com/goldquality/cross-review/releases/latest"

// NewerVersion — latest новее current. Сравниваются только версии релизов vX.Y.Z: у сборки
// без версии (dev) и у брокера без версии напоминания нет.
func NewerVersion(latest, current string) bool {
	l, ok1 := parseVersion(latest)
	c, ok2 := parseVersion(current)
	if !ok1 || !ok2 {
		return false
	}
	for i := range l {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

func parseVersion(s string) ([3]int, bool) {
	var v [3]int
	parts := strings.Split(strings.TrimPrefix(s, "v"), ".")
	if !strings.HasPrefix(s, "v") || len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p != strconv.Itoa(n) {
			return v, false
		}
		v[i] = n
	}
	return v, true
}
