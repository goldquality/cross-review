package broker

import (
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxBody = 64 << 10

// Handler — HTTP API брокера. Все ответы — JSON, ошибки — {"error": "текст для человека"}.
// trustProxy — брокер стоит за прокси (Caddy, Traefik), и адрес клиента берётся из X-Forwarded-For.
func (s *Service) Handler(logger *log.Logger, trustProxy bool) http.Handler {
	h := &api{s: s, log: logger, trustProxy: trustProxy, joins: newLimiter(10, time.Minute), requests: newLimiter(6, time.Minute), calls: newLimiter(240, time.Minute)}
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
	mux.HandleFunc("POST /v1/jobs/{id}/result", h.auth(h.result))
	mux.HandleFunc("POST /v1/requests", h.auth(h.createRequest))
	mux.HandleFunc("GET /v1/requests", h.auth(h.myRequests))
	mux.HandleFunc("GET /v1/me", h.auth(h.me))
	return mux
}

type api struct {
	s                      *Service
	log                    *log.Logger
	trustProxy             bool
	joins, requests, calls *limiter
}

func (h *api) auth(next func(http.ResponseWriter, *http.Request, Member)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			writeErr(w, http.StatusUnauthorized, "нужен заголовок Authorization: Bearer <ключ>")
			return
		}
		m, err := h.s.Auth(strings.TrimSpace(key))
		if err != nil {
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
	if !h.joins.allow(clientIP(r, h.trustProxy)) {
		writeErr(w, http.StatusTooManyRequests, "слишком много попыток входа, подожди минуту")
		return
	}
	var in struct {
		Invite string `json:"invite"`
		Name   string `json:"name"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	out, err := h.s.Join(in.Invite, in.Name)
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
	case errors.Is(err, ErrNameTaken):
		writeErr(w, http.StatusConflict, err.Error())
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

// clientIP — адрес клиента. За прокси это последний адрес X-Forwarded-For (его дописал прокси),
// иначе адрес соединения.
func clientIP(r *http.Request, trustProxy bool) string {
	if xff := r.Header.Get("X-Forwarded-For"); trustProxy && xff != "" {
		if i := strings.LastIndex(xff, ","); i >= 0 {
			return strings.TrimSpace(xff[i+1:])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// limiter — не больше n событий на ключ за окно.
type limiter struct {
	mu     sync.Mutex
	n      int
	window time.Duration
	hits   map[string][]time.Time
}

func newLimiter(n int, window time.Duration) *limiter {
	return &limiter{n: n, window: window, hits: map[string][]time.Time{}}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	cut := now.Add(-l.window)
	h := l.hits[key]
	i := 0
	for i < len(h) && h[i].Before(cut) {
		i++
	}
	h = h[i:]
	if len(h) >= l.n {
		l.hits[key] = h
		return false
	}
	l.hits[key] = append(h, now)
	if len(l.hits) > 10000 {
		for k, v := range l.hits {
			if len(v) == 0 || v[len(v)-1].Before(cut) {
				delete(l.hits, k)
			}
		}
	}
	return true
}
