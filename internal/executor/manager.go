package executor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"

	"agenthub-sandbox/internal/domain"
	"agenthub-sandbox/internal/security"
	"agenthub-sandbox/internal/worktree"
)

type StartRequest struct {
	Command   string            `json:"command"`
	Args      []string          `json:"args,omitempty"`
	Cwd       string            `json:"cwd,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	TimeoutMs int               `json:"timeoutMs,omitempty"`
	Shell     bool              `json:"shell,omitempty"`
	Stdin     string            `json:"stdin,omitempty"`
}

// StartResult 只返回启动信息，真正的输出通过 socket 流式推送。
type StartResult struct {
	ExecID    string    `json:"execId"`
	StartedAt time.Time `json:"startedAt"`
	Cwd       string    `json:"cwd"`
}

type RunResult struct {
	Stdout     string
	Stderr     string
	ExitCode   int
	FinishedAt time.Time
	TimedOut   bool
}

// Callbacks 让 /agents socket 能在执行过程中实时拿到 stdout/stderr/exit。
type Callbacks struct {
	Stdout func(chunk string)
	Stderr func(chunk string)
	Exit   func(result RunResult)
	Error  func(err error)
}

type job struct {
	agentID string
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	cancel  context.CancelFunc
	done    chan RunResult
}

// Manager 管理每个 agent worktree 里的命令执行任务。
type Manager struct {
	registry *worktree.Registry

	mu   sync.RWMutex
	jobs map[string]*job
}

func NewManager(registry *worktree.Registry) *Manager {
	return &Manager{
		registry: registry,
		jobs:     make(map[string]*job),
	}
}

func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var errs []error
	for execID, job := range m.jobs {
		job.cancel()
		if job.cmd != nil && job.cmd.Process != nil {
			errs = append(errs, job.cmd.Process.Kill())
		}
		delete(m.jobs, execID)
	}
	return errors.Join(errs...)
}

// Start 会在 agent 对应的 worktree 内启动一个新进程，并挂上输出回调。
func (m *Manager) Start(agentID string, req StartRequest, callbacks Callbacks) (StartResult, error) {
	state, err := m.registry.MustGet(agentID)
	if err != nil {
		return StartResult{}, err
	}
	if req.Command == "" {
		return StartResult{}, errors.New("command is required")
	}

	cwd, err := resolveCWD(state.RootPath, req.Cwd)
	if err != nil {
		return StartResult{}, err
	}

	execID := uuid.NewString()
	startedAt := time.Now().UTC()

	ctx := context.Background()
	cancel := func() {}
	if req.TimeoutMs > 0 {
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutMs)*time.Millisecond)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}

	cmd := buildCommand(ctx, req)
	cmd.Dir = cwd
	cmd.Env = mergeEnv(req.Env)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return StartResult{}, err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return StartResult{}, err
	}
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return StartResult{}, err
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return StartResult{}, err
	}

	resultCh := make(chan RunResult, 1)
	job := &job{
		agentID: agentID,
		cmd:     cmd,
		stdin:   stdinPipe,
		cancel:  cancel,
		done:    resultCh,
	}

	m.mu.Lock()
	m.jobs[execID] = job
	m.mu.Unlock()

	if err := m.registry.RegisterExec(agentID, execID); err != nil {
		cancel()
		return StartResult{}, err
	}

	var (
		stdoutBuf bytes.Buffer
		stderrBuf bytes.Buffer
		wg        sync.WaitGroup
	)

	wg.Add(2)
	go streamPipe(stdoutPipe, &stdoutBuf, callbacks.Stdout, &wg)
	go streamPipe(stderrPipe, &stderrBuf, callbacks.Stderr, &wg)

	go func() {
		// 在同一个 goroutine 里统一处理等待、收尾和退出回调。
		defer func() {
			cancel()
			m.registry.UnregisterExec(agentID, execID)
			m.mu.Lock()
			delete(m.jobs, execID)
			m.mu.Unlock()
			_ = stdinPipe.Close()
		}()

		if req.Stdin != "" {
			_, _ = io.WriteString(stdinPipe, req.Stdin)
		}
		_ = stdinPipe.Close()

		waitErr := cmd.Wait()
		wg.Wait()

		result := RunResult{
			Stdout:     stdoutBuf.String(),
			Stderr:     stderrBuf.String(),
			FinishedAt: time.Now().UTC(),
			TimedOut:   errors.Is(ctx.Err(), context.DeadlineExceeded),
		}
		if cmd.ProcessState != nil {
			result.ExitCode = cmd.ProcessState.ExitCode()
		}

		if waitErr != nil && !isExitError(waitErr) && callbacks.Error != nil {
			callbacks.Error(waitErr)
		}
		if callbacks.Exit != nil {
			callbacks.Exit(result)
		}
		resultCh <- result
	}()

	return StartResult{
		ExecID:    execID,
		StartedAt: startedAt,
		Cwd:       cwd,
	}, nil
}

// RunBlocking 给兼容 HTTP 端点复用，内部还是走同一套 Start 流程。
func (m *Manager) RunBlocking(agentID string, req StartRequest) (RunResult, error) {
	var result RunResult
	started, err := m.Start(agentID, req, Callbacks{
		Exit: func(runResult RunResult) {
			result = runResult
		},
	})
	if err != nil {
		return RunResult{}, err
	}

	m.mu.RLock()
	job := m.jobs[started.ExecID]
	m.mu.RUnlock()
	if job == nil {
		return result, domain.ErrExecNotFound
	}
	result = <-job.done
	return result, nil
}

func (m *Manager) WriteStdin(execID, chunk string) error {
	m.mu.RLock()
	job := m.jobs[execID]
	m.mu.RUnlock()
	if job == nil {
		return domain.ErrExecNotFound
	}
	if job.stdin == nil {
		return errors.New("stdin is closed")
	}
	_, err := io.WriteString(job.stdin, chunk)
	return err
}

// Kill 根据平台选择结束方式，Windows 直接 Kill，Unix 默认发 TERM。
func (m *Manager) Kill(execID, signalName string) error {
	m.mu.RLock()
	job := m.jobs[execID]
	m.mu.RUnlock()
	if job == nil {
		return domain.ErrExecNotFound
	}
	if job.cmd == nil || job.cmd.Process == nil {
		return domain.ErrExecNotFound
	}
	if runtime.GOOS == "windows" || signalName == "KILL" {
		return job.cmd.Process.Kill()
	}
	return job.cmd.Process.Signal(syscall.SIGTERM)
}

func resolveCWD(rootPath, raw string) (string, error) {
	if raw == "" {
		return rootPath, nil
	}
	rel, err := security.NormalizeRelativePath(raw)
	if err != nil {
		return "", err
	}
	resolved := filepath.Join(rootPath, filepath.FromSlash(rel))
	if err := security.EnsureWithin(rootPath, resolved); err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("cwd is not a directory: %s", raw)
	}
	return resolved, nil
}

func buildCommand(ctx context.Context, req StartRequest) *exec.Cmd {
	if req.Shell {
		// shell 模式主要给兼容接口和需要命令解释器语法的场景使用。
		if runtime.GOOS == "windows" {
			return exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", req.Command)
		}
		return exec.CommandContext(ctx, "sh", "-lc", req.Command)
	}
	return exec.CommandContext(ctx, req.Command, req.Args...)
}

func mergeEnv(additional map[string]string) []string {
	env := os.Environ()
	for key, value := range additional {
		env = append(env, fmt.Sprintf("%s=%s", key, value))
	}
	return env
}

// streamPipe 会边读边转发输出，不等进程结束再一次性返回。
func streamPipe(reader io.Reader, sink *bytes.Buffer, emit func(chunk string), wg *sync.WaitGroup) {
	defer wg.Done()

	buffered := bufio.NewReader(reader)
	buf := make([]byte, 4096)
	for {
		n, err := buffered.Read(buf)
		if n > 0 {
			chunk := string(buf[:n])
			sink.WriteString(chunk)
			if emit != nil {
				emit(chunk)
			}
		}
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			return
		}
	}
}

func isExitError(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr)
}
