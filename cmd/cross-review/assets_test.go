package main

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// Страницы показывают строки брокера. Разметку из строки разбирают только эти свойства и методы —
// в страницах их быть не должно: всё кладётся в textContent.
var htmlSinks = regexp.MustCompile(`innerHTML|outerHTML|insertAdjacentHTML|document\.write|\beval\(|new Function|srcdoc`)

func TestAssetsRenderTextOnly(t *testing.T) {
	walk(t, func(name, body string) {
		if loc := htmlSinks.FindStringIndex(body); loc != nil {
			t.Errorf("%s: %q — строки брокера только через textContent", name, body[loc[0]:loc[1]])
		}
	})
}

// В каждой странице — строгий CSP, и ни одна не грузит ничего с чужих адресов.
func TestAssetsCSPAndNoRemote(t *testing.T) {
	const csp = `default-src 'self'; script-src 'self'; style-src 'self'`
	remote := regexp.MustCompile(`(?i)(src|href)\s*=\s*["']?(https?:)?//|url\(\s*["']?(https?:)?//|import\s.*from\s+["']https?:`)
	pages := 0
	walk(t, func(name, body string) {
		if strings.HasSuffix(name, ".html") {
			pages++
			if !strings.Contains(body, csp) {
				t.Errorf("%s: нет строгого Content-Security-Policy", name)
			}
		}
		if loc := remote.FindStringIndex(body); loc != nil {
			t.Errorf("%s: чужой адрес %q", name, body[loc[0]:loc[1]])
		}
	})
	if pages < 2 {
		t.Fatalf("страниц %d: окно у трея и панель тостов", pages)
	}
}

func walk(t *testing.T, fn func(name, body string)) {
	t.Helper()
	err := fs.WalkDir(assets, "assets", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(assets, p)
		if err != nil {
			return err
		}
		fn(p, string(b))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
