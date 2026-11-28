package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// API — запросы к брокеру. Типы ответов повторяют брокерские, чтобы клиент не тянул SQLite.
type API struct {
	Base string
	Key  string
	HTTP *http.Client
}

// BrokerError — отказ брокера с текстом для человека.
type BrokerError struct {
	Status int
	Msg    string
}

func (e *BrokerError) Error() string { return e.Msg }

// Unauthorized — ключ отозван или не подходит.
func Unauthorized(err error) bool {
	var be *BrokerError
	return errors.As(err, &be) && be.Status == http.StatusUnauthorized
}

// Joined — личный ключ после входа по приглашению.
type Joined struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Balance int    `json:"balance"`
}

// Event — уведомление от брокера.
type Event struct {
	At   int64  `json:"at"`
	Text string `json:"text"`
}

// Beat — ответ на отметку.
type Beat struct {
	Balance    int      `json:"balance"`
	Events     []Event  `json:"events"`
	ServerTime int64    `json:"server_time"`
	Version    string   `json:"version"` // версия брокера = последняя версия клиента
	Team       Team     `json:"team"`
	Ignored    []string `json:"ignored"` // репо из настроек, которых брокер не разрешает
}

// Team — команда по данным брокера: кто онлайн и топ по баллам.
type Team struct {
	Online []Teammate `json:"online"`
	Top    []Teammate `json:"top"`
}

// Teammate — участник команды. State у тех, кто онлайн: free, busy, paused.
type Teammate struct {
	Name    string `json:"name"`
	Balance int    `json:"balance"`
	State   string `json:"state"`
}

// Job — задание: ссылка на MR и кто просит.
type Job struct {
	ID        int64  `json:"job_id"`
	MRURL     string `json:"mr_url"`
	Requester string `json:"requester"`
}

// Requested — ответ на запрос ревью.
type Requested struct {
	ID        int64    `json:"id"`
	Reviewers []string `json:"reviewers"`
	Waiting   bool     `json:"waiting"`
	Balance   int      `json:"balance"`
}

// RequestInfo — мой запрос со статусами заданий.
type RequestInfo struct {
	ID        int64  `json:"id"`
	MRURL     string `json:"mr_url"`
	CreatedAt int64  `json:"created_at"`
	Status    string `json:"status"`
	Jobs      []struct {
		Reviewer string `json:"reviewer"`
		Status   string `json:"status"`
	} `json:"jobs"`
}

// NewAPI — клиент брокера с ключом участника. Редиректам не следует: иначе ключ мог бы уйти по
// http, а тело входа с приглашением — на чужой хост.
func NewAPI(base, key string) *API {
	return &API{Base: base, Key: key, HTTP: &http.Client{
		Timeout:       70 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (a *API) do(ctx context.Context, method, path string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.Base+path, body)
	if err != nil {
		return 0, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a.Key != "" {
		req.Header.Set("Authorization", "Bearer "+a.Key)
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return 0, fmt.Errorf("брокер недоступен: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return resp.StatusCode, &BrokerError{Status: resp.StatusCode, Msg: fmt.Sprintf("брокер ответил редиректом %d на %s: проверь адрес в настройках", resp.StatusCode, resp.Header.Get("Location"))}
	}
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) != nil || e.Error == "" {
			e.Error = fmt.Sprintf("брокер ответил %d", resp.StatusCode)
		}
		return resp.StatusCode, &BrokerError{Status: resp.StatusCode, Msg: e.Error}
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("непонятный ответ брокера: %w", err)
		}
	}
	return resp.StatusCode, nil
}

// Join меняет приглашение на личный ключ.
func (a *API) Join(ctx context.Context, invite, name string) (Joined, error) {
	var out Joined
	_, err := a.do(ctx, "POST", "/v1/join", map[string]string{"invite": invite, "name": name}, &out)
	return out, err
}

// Heartbeat — отметка «онлайн» с проектами и паузой.
func (a *API) Heartbeat(ctx context.Context, repos []string, pausedUntil int64, version string) (Beat, error) {
	var out Beat
	_, err := a.do(ctx, "POST", "/v1/heartbeat", map[string]any{"repos": repos, "paused_until": pausedUntil, "version": version}, &out)
	return out, err
}

// NextJob ждёт задание до wait.
func (a *API) NextJob(ctx context.Context, wait time.Duration) (*Job, error) {
	var out Job
	code, err := a.do(ctx, "GET", "/v1/jobs/next?wait="+strconv.Itoa(int(wait/time.Second)), nil, &out)
	if err != nil || code == http.StatusNoContent {
		return nil, err
	}
	return &out, nil
}

// Accept — участник согласился на ревью.
func (a *API) Accept(ctx context.Context, jobID int64) error {
	_, err := a.do(ctx, "POST", "/v1/jobs/"+strconv.FormatInt(jobID, 10)+"/accept", nil, nil)
	return err
}

// Decline — участник отказался от предложения.
func (a *API) Decline(ctx context.Context, jobID int64) error {
	_, err := a.do(ctx, "POST", "/v1/jobs/"+strconv.FormatInt(jobID, 10)+"/decline", nil, nil)
	return err
}

// Result сообщает итог задания. Хвост вывода уходит брокеру только при неудаче, чтобы было видно,
// что сломалось; вывод удачного ревью остаётся на машине участника.
func (a *API) Result(ctx context.Context, jobID int64, r Result) error {
	tail := r.Tail
	if r.ExitCode == 0 {
		tail = ""
	}
	if len(tail) > 2000 {
		tail = tail[len(tail)-2000:]
	}
	_, err := a.do(ctx, "POST", "/v1/jobs/"+strconv.FormatInt(jobID, 10)+"/result",
		map[string]any{"exit_code": r.ExitCode, "duration_sec": int(r.Duration / time.Second), "error_tail": tail}, nil)
	return err
}

// Request просит ревью MR.
func (a *API) Request(ctx context.Context, mrURL string) (Requested, error) {
	var out Requested
	_, err := a.do(ctx, "POST", "/v1/requests", map[string]string{"mr_url": mrURL}, &out)
	return out, err
}

// Me — баланс и активность участника по данным брокера.
type Me struct {
	Name        string `json:"name"`
	Balance     int    `json:"balance"`
	LastSeen    int64  `json:"last_seen"`
	PausedUntil int64  `json:"paused_until"`
	Repos       int    `json:"repos"`
}

// Whoami — что брокер знает об участнике; отметкой «онлайн» не считается.
func (a *API) Whoami(ctx context.Context) (Me, error) {
	var out Me
	_, err := a.do(ctx, "GET", "/v1/me", nil, &out)
	return out, err
}

// MyRequests — мои последние запросы.
func (a *API) MyRequests(ctx context.Context) ([]RequestInfo, error) {
	var out []RequestInfo
	_, err := a.do(ctx, "GET", "/v1/requests", nil, &out)
	return out, err
}
