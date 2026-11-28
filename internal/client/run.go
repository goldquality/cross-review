package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/goldquality/cross-review/internal/mrurl"
)

// RunTimeout — дольше команда ревью не работает.
const RunTimeout = 60 * time.Minute

const (
	tailBytes = 4000
	keepLogs  = 50
)

// outputGrace — сколько ждать закрытия вывода после выхода команды. Фоновый потомок (например,
// мастер-соединение ssh) может держать вывод открытым часами; без предела клиент ждал бы его и
// засчитал удачное ревью провалом по тайм-ауту. Переменная — для тестов.
var outputGrace = 10 * time.Second

// Result — итог запуска команды ревью.
type Result struct {
	ExitCode int
	Tail     string
	LogPath  string
	Duration time.Duration
}

// Run запускает команду проекта для MR. Ссылка — каноничная, собранная из разобранных частей;
// она подставляется на место <LINK> внутри своего аргумента, процесс стартует без шелла, в папке
// запуска проекта, с закрытым stdin. Отмена ctx или тайм-аут убивают всё дерево процессов.
func Run(ctx context.Context, p Project, mr mrurl.MR, logDir string, env []string) (Result, error) {
	if p.Repo != mr.Repo.String() {
		return Result{}, fmt.Errorf("MR %s не из проекта %s", mr, p.Repo)
	}
	if len(p.Command) == 0 {
		return Result{}, errors.New("у проекта нет команды")
	}
	argv := Substitute(p.Command, mr.String())
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return Result{}, err
	}
	name := time.Now().Format("20060102-150405") + "-" + strings.ReplaceAll(mr.Short(), "!", "-") + ".log"
	logPath := filepath.Join(logDir, name)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return Result{}, err
	}
	defer logFile.Close()
	pruneLogs(logDir)
	fmt.Fprintf(logFile, "$ %s\n(папка %s)\n\n", FormatCommand(argv), p.Workdir)

	ctx, cancel := context.WithTimeout(ctx, RunTimeout)
	defer cancel()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = p.Workdir
	cmd.Stdin = nil
	cmd.Env = env
	tail := &tailBuffer{max: tailBytes}
	out := io.MultiWriter(logFile, tail)
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.WaitDelay = outputGrace
	setProcessGroup(cmd)
	start := time.Now()
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(logFile, "не запустилось: %v\n", err)
		return Result{ExitCode: -1, Tail: "не запустилось: " + err.Error(), LogPath: logPath}, nil
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
	stopped := ""
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		killTree(cmd)
		waitErr = <-done
		stopped = "остановлено"
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			stopped = "остановлено: дольше 60 минут"
		}
		fmt.Fprintln(out, "\n"+stopped)
	}
	res := Result{ExitCode: 0, Tail: tail.String(), LogPath: logPath, Duration: time.Since(start)}
	if stopped != "" {
		res.ExitCode = -1
	} else if waitErr != nil {
		res.ExitCode = -1
		var ee *exec.ExitError
		switch {
		case errors.As(waitErr, &ee):
			res.ExitCode = ee.ExitCode()
		case errors.Is(waitErr, exec.ErrWaitDelay) && cmd.ProcessState != nil:
			// Команда вышла сама, вывод держит её фоновый потомок: итог — код команды.
			res.ExitCode = cmd.ProcessState.ExitCode()
			fmt.Fprintf(logFile, "\n(фоновые процессы команды не закрыли вывод за %s — не жду их)\n", outputGrace)
		}
	}
	fmt.Fprintf(logFile, "\nкод выхода %d за %s\n", res.ExitCode, res.Duration.Round(time.Second))
	return res, nil
}

// tailBuffer хранит последние max байт вывода.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.ToValidUTF8(string(t.b), "")
}

func pruneLogs(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var logs []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".log") {
			logs = append(logs, e.Name())
		}
	}
	sort.Strings(logs)
	for len(logs) > keepLogs {
		os.Remove(filepath.Join(dir, logs[0]))
		logs = logs[1:]
	}
}

// ChildEnv — окружение для команды ревью: текущее, но с PATH из логин-шелла участника. Трей,
// запущенный при входе в систему, получает урезанный PATH, а команде нужны git, just, claude.
func ChildEnv(shellPath string) []string {
	env := os.Environ()
	if shellPath == "" {
		return env
	}
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, "PATH=") {
			out = append(out, kv)
		}
	}
	return append(out, "PATH="+shellPath)
}
