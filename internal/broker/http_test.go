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
	srv := httptest.NewServer(e.s.Handler(log.New(io.Discard, "", 0), false))
	defer srv.Close()

	if code, _ := call(t, srv, "POST", "/v1/join", "", map[string]string{"invite": "nope", "name": "Вася"}); code != http.StatusUnauthorized {
		t.Fatal(code)
	}
	if code, _ := call(t, srv, "POST", "/v1/join", "", map[string]any{"invite": e.inv, "name": "Вася", "extra": 1}); code != http.StatusBadRequest {
		t.Fatal("лишние поля отклоняются:", code)
	}
	code, out := call(t, srv, "POST", "/v1/join", "", map[string]string{"invite": e.inv, "name": "Вася"})
	if code != 200 || out["balance"].(float64) != 2 {
		t.Fatal(code, out)
	}
	vasya := out["key"].(string)
	_, out = call(t, srv, "POST", "/v1/join", "", map[string]string{"invite": e.inv, "name": "Петя"})
	petya := out["key"].(string)

	if code, _ := call(t, srv, "POST", "/v1/heartbeat", "crk_bad", map[string]any{}); code != http.StatusUnauthorized {
		t.Fatal(code)
	}
	for _, k := range []string{vasya, petya} {
		if code, out := call(t, srv, "POST", "/v1/heartbeat", k, map[string]any{"repos": []string{repoKey}, "version": "t"}); code != 200 {
			t.Fatal(code, out)
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

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestLimiter(t *testing.T) {
	l := newLimiter(2, 60e9)
	if !l.allow("a") || !l.allow("a") || l.allow("a") || !l.allow("b") {
		t.Fatal("лимит")
	}
}

func TestHealthzDrain(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "drain")
	t.Setenv("HEALTH_DRAIN_FILE", flag)
	e := newEnv(t)
	srv := httptest.NewServer(e.s.Handler(log.New(io.Discard, "", 0), false))
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
