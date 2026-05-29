package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"agenthub-sandbox/internal/domain"
	"agenthub-sandbox/internal/executor"
	"agenthub-sandbox/internal/filesystem"
	"agenthub-sandbox/internal/gitmgr"
)

type Router struct {
	fs  *filesystem.Service
	exe *executor.Manager
	git *gitmgr.Manager
}

// Router 承载 git 编排接口和两个兼容旧链路的 HTTP 端点。
func New(fs *filesystem.Service, exe *executor.Manager, git *gitmgr.Manager) *Router {
	return &Router{fs: fs, exe: exe, git: git}
}

func (r *Router) Register(mux *http.ServeMux) {
	// 这里把 sandbox 对外暴露的 HTTP 路径一次性注册进去。
	mux.HandleFunc("GET /health", r.handleHealth)
	mux.HandleFunc("POST /execute", r.handleExecute)
	mux.HandleFunc("/download/", r.handleDownload)
	mux.HandleFunc("GET /git/agents", r.handleListAgents)
	mux.HandleFunc("GET /git/agents/{agentId}/status", r.handleStatus)
	mux.HandleFunc("GET /git/agents/{agentId}/diff", r.handleDiff)
	mux.HandleFunc("POST /git/agents/{agentId}/complete", r.handleComplete)
	mux.HandleFunc("POST /git/agents/{agentId}/sync", r.handleSync)
	mux.HandleFunc("POST /git/agents/{agentId}/merge", r.handleMerge)
	mux.HandleFunc("POST /git/agents/{agentId}/merge/abort", r.handleAbortMerge)
	mux.HandleFunc("POST /git/agents/{agentId}/promote", r.handlePromote)
	mux.HandleFunc("DELETE /git/agents/{agentId}/worktree", r.handleDeleteWorktree)
}

func (r *Router) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeText(w, http.StatusOK, "ok")
}

// /execute 是给现有 gateway / adaptor 兼容保留的阻塞式执行接口。
func (r *Router) handleExecute(w http.ResponseWriter, req *http.Request) {
	agentID, err := agentIDFromRequest(req)
	if err != nil {
		writeError(w, err)
		return
	}
	var body struct {
		Command string `json:"command"`
	}
	if err := readJSON(req, &body); err != nil {
		writeError(w, err)
		return
	}
	result, err := r.exe.RunBlocking(agentID, executor.StartRequest{
		Command: body.Command,
		Shell:   true,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"stdout":    result.Stdout,
		"stderr":    result.Stderr,
		"exit_code": result.ExitCode,
	})
}

// /download 直接复用 filesystem 的安全读文件逻辑。
func (r *Router) handleDownload(w http.ResponseWriter, req *http.Request) {
	agentID, err := agentIDFromRequest(req)
	if err != nil {
		writeError(w, err)
		return
	}
	rawPath := strings.TrimPrefix(req.URL.EscapedPath(), "/download/")
	rawPath, err = url.PathUnescape(rawPath)
	if err != nil {
		writeError(w, err)
		return
	}
	read, err := r.fs.Read(agentID, rawPath, 0, 0)
	if err != nil {
		writeError(w, err)
		return
	}
	writeText(w, http.StatusOK, read.Content)
}

func (r *Router) handleListAgents(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"items": r.git.ListAgents(),
	})
}

func (r *Router) handleStatus(w http.ResponseWriter, req *http.Request) {
	status, err := r.git.Status(req.PathValue("agentId"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (r *Router) handleDiff(w http.ResponseWriter, req *http.Request) {
	diff, err := r.git.Diff(req.PathValue("agentId"), req.URL.Query().Get("base"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, diff)
}

func (r *Router) handleComplete(w http.ResponseWriter, req *http.Request) {
	var body gitmgr.CompleteRequest
	if err := readJSON(req, &body); err != nil {
		writeError(w, err)
		return
	}
	result, err := r.git.Complete(req.PathValue("agentId"), body)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (r *Router) handleMerge(w http.ResponseWriter, req *http.Request) {
	var body gitmgr.MergeRequest
	if err := readJSON(req, &body); err != nil {
		writeError(w, err)
		return
	}
	result, err := r.git.Merge(req.PathValue("agentId"), body)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (r *Router) handleSync(w http.ResponseWriter, req *http.Request) {
	var body gitmgr.SyncRequest
	if err := readJSON(req, &body); err != nil {
		writeError(w, err)
		return
	}
	result, err := r.git.Sync(req.PathValue("agentId"), body)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (r *Router) handleAbortMerge(w http.ResponseWriter, req *http.Request) {
	if err := r.git.AbortMerge(req.PathValue("agentId")); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (r *Router) handlePromote(w http.ResponseWriter, req *http.Request) {
	var body gitmgr.PromoteRequest
	if err := readJSON(req, &body); err != nil {
		writeError(w, err)
		return
	}
	result, err := r.git.Promote(req.PathValue("agentId"), body)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (r *Router) handleDeleteWorktree(w http.ResponseWriter, req *http.Request) {
	agentID := req.PathValue("agentId")
	_ = r.fs.RemoveAgent(agentID)
	if err := r.git.DeleteWorktree(agentID); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func agentIDFromRequest(req *http.Request) (string, error) {
	agentID := strings.TrimSpace(req.Header.Get("X-AgentHub-Agent-Id"))
	if agentID == "" {
		agentID = strings.TrimSpace(req.URL.Query().Get("agentId"))
	}
	if agentID == "" {
		return "", domain.ErrAgentIDRequired
	}
	return agentID, nil
}

func readJSON(req *http.Request, target any) error {
	defer req.Body.Close()
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return err
	}
	if len(body) == 0 {
		body = []byte("{}")
	}
	return json.Unmarshal(body, target)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// writeError 负责把内部错误映射成稳定的 HTTP 状态码和错误码。
func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	code := "INTERNAL_ERROR"
	switch {
	case errors.Is(err, domain.ErrAgentIDRequired), errors.Is(err, domain.ErrInvalidPath), errors.Is(err, domain.ErrPathEscapesWorktree):
		status = http.StatusBadRequest
		code = "BAD_REQUEST"
	case errors.Is(err, domain.ErrWorktreeNotPrepared), errors.Is(err, domain.ErrExecNotFound):
		status = http.StatusNotFound
		code = "NOT_FOUND"
	case errors.Is(err, domain.ErrVersionConflict), errors.Is(err, domain.ErrGitNoChanges), errors.Is(err, domain.ErrMergeConflict):
		status = http.StatusConflict
		code = "CONFLICT"
	}
	writeJSON(w, status, domain.APIError{
		Code:    code,
		Message: err.Error(),
	})
}
