package broker

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxBody = 64 << 10

// Handler — HTTP API брокера. Все ответы — JSON, ошибки — {"error": "текст для человека"}.
// proxies — адреса прокси (Caddy, Traefik): только от них брокер верит X-Forwarded-For.
func (s *Service) Handler(logger *log.Logger, proxies []netip.Prefix) http.Handler {
	h := &api{s: s, log: logger, proxies: proxies,
		joins: newLimiter(10, time.Minute), badKeys: newLimiter(30, time.Minute),
		requests: newLimiter(6, time.Minute), calls: newLimiter(240, time.Minute)}
	mux := http.NewServeMux()
	// HEALTH_DRAIN_FILE — флаг перед остановкой реплики: пока файл есть, /healthz отвечает 503, и
	// прокси перестаёт слать сюда запросы, а новая реплика рядом уже принимает их.
	drain := os.Getenv("HEALTH_DRAIN_FILE")
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if drain != "" {
			if _, err := os.Stat(drain); err == nil {
				http.Error(w, "drain", http.StatusServiceUnavailable)
				return
			}
		}
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("POST /v1/join", h.join)
	mux.HandleFunc("POST /v1/heartbeat", h.auth(h.heartbeat))
	mux.HandleFunc("GET /v1/jobs/next", h.auth(h.nextJob))
	mux.HandleFunc("POST /v1/jobs/{id}/accept", h.auth(h.jobAction(h.s.Accept, "принял")))
	mux.HandleFunc("POST /v1/jobs/{id}/decline", h.auth(h.jobAction(h.s.Decline, "отказался")))
	mux.HandleFunc("POST /v1/jobs/{id}/result", h.auth(h.result))
	mux.HandleFunc("POST /v1/requests", h.auth(h.createRequest))
	mux.HandleFunc("GET /v1/requests", h.auth(h.myRequests))
	mux.HandleFunc("GET /v1/me", h.auth(h.me))
	return mux
}

type api struct {
	s                               *Service
	log                             *log.Logger
	proxies                         []netip.Prefix
	joins, badKeys, requests, calls *limiter
}

func (h *api) auth(next func(http.ResponseWriter, *http.Request, Member)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			writeErr(w, http.StatusUnauthorized, "нужен заголовок Authorization: Bearer <ключ>")
			return
		}
		// Перебор ключей с одного адреса упирается в лимит раньше, чем в базу.
		ip := clientIP(r, h.proxies)
		if h.badKeys.full(ip) {
			writeErr(w, http.StatusTooManyRequests, "слишком много неверных ключей, подожди минуту")
			return
		}
		m, err := h.s.Auth(strings.TrimSpace(key))
		if err != nil {
			if errors.Is(err, ErrBadKey) {
				h.badKeys.allow(ip)
			}
			h.fail(w, err)
			return
		}
		if !h.calls.allow(strconv.FormatInt(m.ID, 10)) {
			writeErr(w, http.StatusTooManyRequests, "слишком часто, подожди минуту")
			return
		}
		next(w, r, m)
	}
}

func (h *api) join(w http.ResponseWriter, r *http.Request) {
	if !h.joins.allow(clientIP(r, h.proxies)) {
		writeErr(w, http.StatusTooManyRequests, "слишком много попыток входа, подожди минуту")
		return
	}
	var in struct {
		Invite string `json:"invite"`
		Name   string `json:"name"` // клиенты до v0.1.4 присылают имя; его задаёт приглашение
	}
	if !readJSON(w, r, &in) {
		return
	}
	out, err := h.s.Join(in.Invite)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.log.Printf("join: %s", out.Name)
	writeJSON(w, http.StatusOK, out)
}

func (h *api) heartbeat(w http.ResponseWriter, r *http.Request, m Member) {
	var in struct {
		Repos       []string `json:"repos"`
		PausedUntil int64    `json:"paused_until"`
		Version     string   `json:"version"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	out, err := h.s.Heartbeat(m, in.Repos, in.PausedUntil, in.Version)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *api) nextJob(w http.ResponseWriter, r *http.Request, m Member) {
	wait := 30 * time.Second
	if v := r.URL.Query().Get("wait"); v != "" {
		n, err := strconv.Atoi(strings.TrimSuffix(v, "s"))
		if err != nil || n < 0 || n > 50 {
			writeErr(w, http.StatusBadRequest, "wait — секунды от 0 до 50")
			return
		}
		wait = time.Duration(n) * time.Second
	}
	job, err := h.s.NextJob(r.Context(), m, wait)
	if err != nil {
		h.fail(w, err)
		return
	}
	if job == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h.log.Printf("job %d → %s: %s", job.ID, m.Name, job.MRURL)
	writeJSON(w, http.StatusOK, job)
}

// jobAction — ответ участника на задание без тела запроса: принять или отказаться.
func (h *api) jobAction(do func(Member, int64) error, verb string) func(http.ResponseWriter, *http.Request, Member) {
	return func(w http.ResponseWriter, r *http.Request, m Member) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "номер задания — число")
			return
		}
		if err := do(m, id); err != nil {
			h.fail(w, err)
			return
		}
		h.log.Printf("job %d: %s %s", id, m.Name, verb)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h *api) result(w http.ResponseWriter, r *http.Request, m Member) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "номер задания — число")
		return
	}
	var in struct {
		ExitCode    int    `json:"exit_code"`
		DurationSec int    `json:"duration_sec"`
		ErrorTail   string `json:"error_tail"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if err := h.s.Result(m, id, in.ExitCode, in.ErrorTail); err != nil {
		h.fail(w, err)
		return
	}
	h.log.Printf("job %d ← %s: код %d за %d с", id, m.Name, in.ExitCode, in.DurationSec)
	w.WriteHeader(http.StatusNoContent)
}

func (h *api) createRequest(w http.ResponseWriter, r *http.Request, m Member) {
	if !h.requests.allow(strconv.FormatInt(m.ID, 10)) {
		writeErr(w, http.StatusTooManyRequests, "слишком много запросов ревью, подожди минуту")
		return
	}
	var in struct {
		MRURL string `json:"mr_url"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	out, err := h.s.CreateRequest(m, in.MRURL)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.log.Printf("request %d от %s: %s → %v", out.ID, m.Name, in.MRURL, out.Reviewers)
	writeJSON(w, http.StatusOK, out)
}

func (h *api) myRequests(w http.ResponseWriter, r *http.Request, m Member) {
	out, err := h.s.MyRequests(m)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *api) me(w http.ResponseWriter, r *http.Request, m Member) {
	out, err := h.s.Whoami(m)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *api) fail(w http.ResponseWriter, err error) {
	var ue *UserError
	switch {
	case errors.As(err, &ue):
		writeErr(w, http.StatusUnprocessableEntity, ue.Msg)
	case errors.Is(err, ErrBadInvite), errors.Is(err, ErrBadKey):
		writeErr(w, http.StatusUnauthorized, err.Error())
	default:
		h.log.Printf("ошибка: %v", err)
		writeErr(w, http.StatusInternalServerError, "внутренняя ошибка брокера")
	}
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "тело запроса — JSON по схеме API")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// clientIP — адрес клиента. Если соединение пришло от прокси из proxies, это последний адрес
// X-Forwarded-For: его дописал сам прокси, а всё левее мог прислать кто угодно.
func clientIP(r *http.Request, proxies []netip.Prefix) string {
	remote := r.RemoteAddr
	if host, _, err := net.SplitHostPort(remote); err == nil {
		remote = host
	}
	addr, err := netip.ParseAddr(remote)
	if err != nil || !inPrefixes(addr.Unmap(), proxies) {
		return remote
	}
	values := r.Header.Values("X-Forwarded-For")
	if len(values) == 0 {
		return remote
	}
	last := values[len(values)-1]
	if i := strings.LastIndex(last, ","); i >= 0 {
		last = last[i+1:]
	}
	if fwd, err := netip.ParseAddr(strings.TrimSpace(last)); err == nil {
		return fwd.Unmap().String()
	}
	return remote
}

func inPrefixes(a netip.Addr, list []netip.Prefix) bool {
	for _, p := range list {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// PrivateNets — частные сети и localhost: там живёт прокси в Docker.
var PrivateNets = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("fc00::/7"), netip.MustParsePrefix("::1/128"),
}

// ParseProxies разбирает список подсетей прокси через запятую: «172.18.0.0/16, 10.0.0.5».
func ParseProxies(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "/") {
			a, err := netip.ParseAddr(part)
			if err != nil {
				return nil, fmt.Errorf("адрес прокси %q: %w", part, err)
			}
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
			continue
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, fmt.Errorf("подсеть прокси %q: %w", part, err)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// limiter — не больше n событий на ключ за окно. Старые ключи вычищаются раз в окно, поэтому
// память ограничена числом ключей за последнюю минуту.
type limiter struct {
	mu     sync.Mutex
	n      int
	window time.Duration
	hits   map[string][]time.Time
	swept  time.Time
}

func newLimiter(n int, window time.Duration) *limiter {
	return &limiter{n: n, window: window, hits: map[string][]time.Time{}}
}

// recent — попадания ключа за окно; заодно раз в окно убирает ключи без попаданий.
func (l *limiter) recent(key string, now time.Time) []time.Time {
	cut := now.Add(-l.window)
	if now.Sub(l.swept) > l.window {
		for k, v := range l.hits {
			if len(v) == 0 || !v[len(v)-1].After(cut) {
				delete(l.hits, k)
			}
		}
		l.swept = now
	}
	h := l.hits[key]
	i := 0
	for i < len(h) && !h[i].After(cut) {
		i++
	}
	return h[i:]
}

// allow засчитывает событие, если лимит не исчерпан.
func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	h := l.recent(key, now)
	if len(h) >= l.n {
		l.hits[key] = h
		return false
	}
	l.hits[key] = append(h, now)
	return true
}

// full — лимит исчерпан; событие не засчитывается.
func (l *limiter) full(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	h := l.recent(key, time.Now())
	if len(h) == 0 {
		delete(l.hits, key)
		return false
	}
	l.hits[key] = h
	return len(h) >= l.n
}
