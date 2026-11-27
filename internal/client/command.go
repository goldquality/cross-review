package client

import (
	"errors"
	"strings"
)

// LinkToken — место ссылки на MR в команде ревью.
const LinkToken = "<LINK>"

// ParseCommand разбивает команду на аргументы: пробелы разделяют, '…' и "…" группируют. Обратная
// косая черта — обычный символ, чтобы пути Windows работали как есть. Шелл не участвует: команда
// потом запускается напрямую.
func ParseCommand(s string) ([]string, error) {
	var (
		args  []string
		cur   strings.Builder
		quote rune
		have  bool
	)
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			have = true
		case r == ' ' || r == '\t':
			if have {
				args = append(args, cur.String())
				cur.Reset()
				have = false
			}
		case r == '\n' || r == '\r':
			return nil, errors.New("команда — одна строка")
		default:
			cur.WriteRune(r)
			have = true
		}
	}
	if quote != 0 {
		return nil, errors.New("в команде не закрыта кавычка")
	}
	if have {
		args = append(args, cur.String())
	}
	if len(args) == 0 || args[0] == "" {
		return nil, errors.New("команда пустая")
	}
	if strings.Contains(args[0], LinkToken) {
		return nil, errors.New("<LINK> не может быть программой")
	}
	found := false
	for _, a := range args {
		if strings.Contains(a, LinkToken) {
			found = true
		}
	}
	if !found {
		return nil, errors.New("в команде нет <LINK> — места для ссылки на MR")
	}
	return args, nil
}

// Substitute подставляет ссылку на место <LINK>. Ссылка всегда остаётся внутри своего аргумента.
func Substitute(argv []string, link string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = strings.ReplaceAll(a, LinkToken, link)
	}
	return out
}

// FormatCommand — команда для показа человеку.
func FormatCommand(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		if a == "" || strings.ContainsAny(a, " \t'\"") {
			if strings.Contains(a, "'") {
				parts[i] = `"` + a + `"`
			} else {
				parts[i] = "'" + a + "'"
			}
		} else {
			parts[i] = a
		}
	}
	return strings.Join(parts, " ")
}
