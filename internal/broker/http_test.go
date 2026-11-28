package broker

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func call(t *testing.T, srv *httptest.Server, method, path, key string, body any) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, srv.URL+path, r)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestHTTPFlow(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewServer(e.s.Handler(log.New(io.Discard, "", 0), nil))
	defer srv.Close()

	if code, _ := call(t, srv, "POST", "/v1/join", "", map[string]string{"invite": "nope", "name": "Вася"}); code != http.StatusUnauthorized {
		t.Fatal(code)
	}
	inv := e.invite("Вася")
	if code, _ := call(t, srv, "POST", "/v1/join", "", map[string]any{"invite": inv, "extra": 1}); code != http.StatusBadRequest {
		t.Fatal("лишние поля отклоняются:", code)
	}
	// Клиенты до v0.1.4 присылают имя; его задаёт приглашение.
	code, out := call(t, srv, "POST", "/v1/join", "", map[string]string{"invite": inv, "name": "Самозванец"})
	if code != 200 || out["balance"].(float64) != 2 || out["name"] != "Вася" {
		t.Fatal(code, out)
	}
	vasya := out["key"].(string)
	_, out = call(t, srv, "POST", "/v1/join", "", map[string]string{"invite": e.invite("Петя")})
	petya := out["key"].(string)

	if code, _ := call(t, srv, "POST", "/v1/heartbeat", "crk_bad", map[string]any{}); code != http.StatusUnauthorized {
		t.Fatal(code)
	}
	e.s.Version = "v1.2.3"
	for _, k := range []string{vasya, petya} {
		code, out := call(t, srv, "POST", "/v1/heartbeat", k, map[string]any{"repos": []string{repoKey}, "version": "t"})
		if code != 200 || out["version"] != "v1.2.3" {
			t.Fatal("отметка отдаёт версию брокера:", code, out)
		}
	}
	if code, out := call(t, srv, "POST", "/v1/heartbeat", vasya, map[string]any{"repos": []string{"../etc"}}); code != http.StatusUnprocessableEntity {
		t.Fatal(code, out)
	}
	call(t, srv, "POST", "/v1/heartbeat", vasya, map[string]any{"repos": []string{repoKey}})

	code, out = call(t, srv, "POST", "/v1/requests", vasya, map[string]string{"mr_url": mr9})
	if code != 200 || len(out["reviewers"].([]any)) != 1 {
		t.Fatal(code, out)
	}
	code, out = call(t, srv, "GET", "/v1/jobs/next?wait=0", petya, nil)
	if code != 200 || out["mr_url"] != mr9 {
		t.Fatal(code, out)
	}
	id := int(out["job_id"].(float64))
	if code, _ := call(t, srv, "GET", "/v1/jobs/next?wait=0", petya, nil); code != http.StatusNoContent {
		t.Fatal(code)
	}
	if code, _ := call(t, srv, "GET", "/v1/jobs/next?wait=999", petya, nil); code != http.StatusBadRequest {
		t.Fatal(code)
	}
	if code, _ := call(t, srv, "POST", "/v1/jobs/"+itoa(id)+"/accept", vasya, nil); code != http.StatusUnprocessableEntity {
		t.Fatal("принять чужое задание:", code)
	}
	if code, out := call(t, srv, "POST", "/v1/jobs/"+itoa(id)+"/accept", petya, nil); code != http.StatusNoContent {
		t.Fatal("принять:", code, out)
	}
	_, out = call(t, srv, "POST", "/v1/heartbeat", vasya, map[string]any{"repos": []string{repoKey}})
	team, _ := out["team"].(map[string]any)
	if online, _ := team["online"].([]any); len(online) != 2 {
		t.Fatal("в отметке команда:", out)
	}
	path := "/v1/jobs/" + itoa(id) + "/result"
	if code, _ := call(t, srv, "POST", path, vasya, map[string]any{"exit_code": 0}); code != http.StatusUnprocessableEntity {
		t.Fatal("чужое задание:", code)
	}
	if code, out := call(t, srv, "POST", path, petya, map[string]any{"exit_code": 0, "duration_sec": 5}); code != http.StatusNoContent {
		t.Fatal(code, out)
	}
	_, out = call(t, srv, "POST", "/v1/heartbeat", petya, map[string]any{"repos": []string{repoKey}})
	if out["balance"].(float64) != 3 {
		t.Fatal(out)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/v1/requests", nil)
	req.Header.Set("Authorization", "Bearer "+vasya)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var list []RequestInfo
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list) != 1 || list[0].Status != "done" || list[0].Jobs[0].Reviewer != "Петя" {
		t.Fatal(list)
	}
}

func TestHTTPDecline(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewServer(e.s.Handler(log.New(io.Discard, "", 0), nil))
	defer srv.Close()
	keys := map[string]string{}
	for _, n := range []string{"Вася", "Петя"} {
		_, out := call(t, srv, "POST", "/v1/join", "", map[string]string{"invite": e.invite(n)})
		keys[n] = out["key"].(string)
		call(t, srv, "POST", "/v1/heartbeat", keys[n], map[string]any{"repos": []string{repoKey}})
	}
	call(t, srv, "POST", "/v1/requests", keys["Вася"], map[string]string{"mr_url": mr9})
	_, out := call(t, srv, "GET", "/v1/jobs/next?wait=0", keys["Петя"], nil)
	id := itoa(int(out["job_id"].(float64)))
	if code, _ := call(t, srv, "POST", "/v1/jobs/x/decline", keys["Петя"], nil); code != http.StatusBadRequest {
		t.Fatal(code)
	}
	if code, out := call(t, srv, "POST", "/v1/jobs/"+id+"/decline", keys["Петя"], nil); code != http.StatusNoContent {
		t.Fatal(code, out)
	}
	if code, _ := call(t, srv, "POST", "/v1/jobs/"+id+"/decline", keys["Петя"], nil); code != http.StatusUnprocessableEntity {
		t.Fatal("повторный отказ:", code)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestBadKeysLimitedPerIP(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewServer(e.s.Handler(log.New(io.Discard, "", 0), nil))
	defer srv.Close()
	good := e.joinKey("Петя").Key
	codes := map[int]int{}
	for i := 0; i < 35; i++ {
		code, _ := call(t, srv, "GET", "/v1/me", "crk_wrong"+itoa(i), nil)
		codes[code]++
	}
	if codes[http.StatusUnauthorized] != 30 || codes[http.StatusTooManyRequests] != 5 {
		t.Fatal("перебор ключей упирается в лимит:", codes)
	}
	if code, _ := call(t, srv, "GET", "/v1/me", good, nil); code != http.StatusTooManyRequests {
		t.Fatal("адрес с перебором ждёт минуту и с верным ключом:", code)
	}
}

func TestClientIP(t *testing.T) {
	proxies, err := ParseProxies("172.18.0.0/16, 10.0.0.5")
	if err != nil {
		t.Fatal(err)
	}
	req := func(remote string, xff ...string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		for _, v := range xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		return r
	}
	for _, c := range []struct {
		r    *http.Request
		want string
	}{
		{req("172.18.0.3:5000", "6.6.6.6, 1.2.3.4"), "1.2.3.4"},            // прокси дописал адрес последним
		{req("172.18.0.3:5000", "9.9.9.9", "6.6.6.6, 1.2.3.4"), "1.2.3.4"}, // несколько строк — последняя
		{req("10.0.0.5:5000", "1.2.3.4"), "1.2.3.4"},                       // одиночный адрес прокси
		{req("8.8.8.8:5000", "1.2.3.4"), "8.8.8.8"},                        // не прокси — заголовку не верим
		{req("172.19.0.3:5000", "1.2.3.4"), "172.19.0.3"},                  // чужая подсеть Docker
		{req("172.18.0.3:5000", "not-an-ip"), "172.18.0.3"},                // мусор в заголовке
		{req("172.18.0.3:5000"), "172.18.0.3"},                             // без заголовка
		{req("[::ffff:172.18.0.3]:5000", "1.2.3.4"), "1.2.3.4"},            // IPv4 в IPv6
	} {
		if got := clientIP(c.r, proxies); got != c.want {
			t.Errorf("%s %v: %s, want %s", c.r.RemoteAddr, c.r.Header.Values("X-Forwarded-For"), got, c.want)
		}
	}
	if got := clientIP(req("172.18.0.3:5000", "1.2.3.4"), nil); got != "172.18.0.3" {
		t.Fatal("без списка прокси заголовку не верим:", got)
	}
	if _, err := ParseProxies("10.0.0.0/33"); err == nil {
		t.Fatal("неверная подсеть")
	}
}

func TestLimiter(t *testing.T) {
	l := newLimiter(2, 60e9)
	if !l.allow("a") || !l.allow("a") || l.allow("a") || !l.allow("b") {
		t.Fatal("лимит")
	}
	if !l.full("a") || l.full("b") || l.full("c") {
		t.Fatal("full не засчитывает событие")
	}
	// Старые ключи вычищаются раз в окно.
	l2 := newLimiter(1, 10*time.Millisecond)
	for i := 0; i < 100; i++ {
		l2.allow(itoa(i))
	}
	time.Sleep(20 * time.Millisecond)
	l2.allow("x")
	if len(l2.hits) != 1 {
		t.Fatal("ключей:", len(l2.hits))
	}
}

func TestHealthzDrain(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "drain")
	t.Setenv("HEALTH_DRAIN_FILE", flag)
	e := newEnv(t)
	srv := httptest.NewServer(e.s.Handler(log.New(io.Discard, "", 0), nil))
	defer srv.Close()
	get := func() int {
		resp, err := http.Get(srv.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := get(); code != 200 {
		t.Fatal("без флага:", code)
	}
	os.WriteFile(flag, nil, 0o644)
	if code := get(); code != http.StatusServiceUnavailable {
		t.Fatal("с флагом drain:", code)
	}
}
