package client

import (
	"strings"
	"unicode"
)

const maxShown = 300

// Clean — текст от брокера, готовый к показу в уведомлении или терминале. Брокер в интернете,
// поэтому его строкам не верим:
//   - на Windows zenity вставляет текст уведомления в скрипт PowerShell внутри @"…"@, где $(…)
//     выполняется, ` экранирует, а "@ в начале строки закрывает строку;
//   - на Linux тело уведомления понимает разметку <…>;
//   - в терминале управляющие последовательности переписывают экран.
//
// Поэтому убираем $, `, управляющие и невидимые символы, двойные кавычки меняем на одинарные,
// угловые скобки — на ‹›, переводы строк — на пробел, длину ограничиваем.
func Clean(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n == maxShown {
			b.WriteRune('…')
			break
		}
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			r = ' '
		case r == '$' || r == '`' || unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			continue
		case r == '"' || r == '“' || r == '”' || r == '„':
			r = '\''
		case r == '<':
			r = '‹'
		case r == '>':
			r = '›'
		case r == '&':
			r = '+'
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}
