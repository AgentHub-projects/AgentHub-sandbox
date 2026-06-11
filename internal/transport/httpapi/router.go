package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
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
	mux.HandleFunc("GET /read/", r.handleRead)
	mux.HandleFunc("POST /write/", r.handleWrite)
	mux.HandleFunc("GET /git/agents", r.handleListAgents)
	mux.HandleFunc("GET /git/agents/{agentId}/status", r.handleStatus)
	mux.HandleFunc("GET /git/agents/{agentId}/diff", r.handleDiff)
	mux.HandleFunc("POST /git/agents/{agentId}/images/manifest", r.handleUpsertAgentImageManifest)
	mux.HandleFunc("GET /filesystem/git/main/commits", r.handleMainCommits)
	mux.HandleFunc("GET /filesystem/git/main/commits/{commitSha}/diff/files", r.handleMainCommitDiffFiles)
	mux.HandleFunc("GET /filesystem/git/main/commits/{commitSha}/diff/file", r.handleMainCommitDiffFile)
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
	rawPath := strings.TrimPrefix(req.URL.EscapedPath(), "/download/")
	rawPath, err := url.PathUnescape(rawPath)
	if err != nil {
		writeError(w, err)
		return
	}
	if preview, err := r.fs.ImagePreview(domain.MainWorkspaceID, rawPath); err != nil {
		writeError(w, err)
		return
	} else if preview != nil {
		http.Redirect(w, req, preview.PreviewURL, http.StatusFound)
		return
	}

	read, err := r.fs.ReadBytes(domain.MainWorkspaceID, rawPath)
	if err != nil {
		writeError(w, err)
		return
	}
	writeFileBytes(w, read)
}

func (r *Router) handleRead(w http.ResponseWriter, req *http.Request) {
	rawPath, err := pathAfter(req, "/read/")
	if err != nil {
		writeError(w, err)
		return
	}

	agentID := agentIDFromRequestOrMain(req)
	read, err := r.fs.Read(agentID, rawPath, readLineStart(req), readLineEnd(req))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, read)
}

func (r *Router) handleWrite(w http.ResponseWriter, req *http.Request) {
	rawPath, err := pathAfter(req, "/write/")
	if err != nil {
		writeError(w, err)
		return
	}

	var body struct {
		Content         *string `json:"content"`
		ContentBase64   string  `json:"contentBase64"`
		ExpectedVersion string  `json:"expectedVersion"`
		CreateDirs      *bool   `json:"createDirs"`
	}
	if err := readJSON(req, &body); err != nil {
		writeError(w, err)
		return
	}

	var content []byte
	switch {
	case body.ContentBase64 != "":
		decoded, err := base64.StdEncoding.DecodeString(body.ContentBase64)
		if err != nil {
			writeError(w, domain.ErrInvalidPath)
			return
		}
		content = decoded
	case body.Content != nil:
		content = []byte(*body.Content)
	default:
		writeError(w, domain.ErrInvalidPath)
		return
	}

	createDirs := true
	if body.CreateDirs != nil {
		createDirs = *body.CreateDirs
	}
	written, err := r.fs.WriteBytes(agentIDFromRequestOrMain(req), rawPath, content, body.ExpectedVersion, createDirs, "agent")
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, written)
}

func (r *Router) handleUpsertAgentImageManifest(w http.ResponseWriter, req *http.Request) {
	var body domain.ImagePreview
	if err := readJSON(req, &body); err != nil {
		writeError(w, err)
		return
	}
	preview, err := r.fs.UpsertImagePreview(req.PathValue("agentId"), body)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"preview": preview,
	})
}

func writeFileBytes(w http.ResponseWriter, read filesystem.ReadBytesResult) {
	contentType := http.DetectContentType(read.Content)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(read.Content)))
	w.Header().Set("ETag", read.Version)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(read.Content)
}

func (r *Router) handleListAgents(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"items": r.git.ListAgents(),
	})
}

func (r *Router) handleMainCommits(w http.ResponseWriter, req *http.Request) {
	limit := 100
	if rawLimit := strings.TrimSpace(req.URL.Query().Get("limit")); rawLimit != "" {
		if parsed, err := strconv.Atoi(rawLimit); err == nil {
			limit = parsed
		}
	}
	result, err := r.git.ListMainCommits(limit, req.URL.Query().Get("cursor"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (r *Router) handleMainCommitDiffFiles(w http.ResponseWriter, req *http.Request) {
	result, err := r.git.MainCommitDiffFiles(req.PathValue("commitSha"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (r *Router) handleMainCommitDiffFile(w http.ResponseWriter, req *http.Request) {
	result, err := r.git.MainCommitDiffFile(req.PathValue("commitSha"), req.URL.Query().Get("path"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
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

func agentIDFromRequestOrMain(req *http.Request) string {
	agentID := strings.TrimSpace(req.Header.Get("X-AgentHub-Agent-Id"))
	if agentID == "" {
		return domain.MainWorkspaceID
	}
	return agentID
}

func readLineStart(req *http.Request) int {
	if value, ok := readPositiveInt(req, "lineStart"); ok {
		return value
	}
	if value, ok := readPositiveInt(req, "line"); ok {
		return value
	}
	return 0
}

func readLineEnd(req *http.Request) int {
	if value, ok := readPositiveInt(req, "lineEnd"); ok {
		return value
	}
	start := readLineStart(req)
	limit, ok := readPositiveInt(req, "limit")
	if !ok || start <= 0 {
		return 0
	}
	return start + limit - 1
}

func readPositiveInt(req *http.Request, key string) (int, bool) {
	raw := strings.TrimSpace(req.URL.Query().Get(key))
	if raw == "" {
		return 0, false
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, false
	}
	return value, true
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

func pathAfter(req *http.Request, marker string) (string, error) {
	parts := strings.SplitN(req.URL.EscapedPath(), marker, 2)
	if len(parts) != 2 {
		return "", domain.ErrInvalidPath
	}
	rawPath, err := url.PathUnescape(parts[1])
	if err != nil {
		return "", err
	}
	return rawPath, nil
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
	case errors.Is(err, domain.ErrInvalidCommit):
		status = http.StatusBadRequest
		code = "INVALID_COMMIT"
	case errors.Is(err, domain.ErrInvalidPath), errors.Is(err, domain.ErrPathEscapesWorktree):
		status = http.StatusBadRequest
		code = "INVALID_PATH"
	case errors.Is(err, domain.ErrAgentIDRequired):
		status = http.StatusBadRequest
		code = "AGENT_ID_REQUIRED"
	case errors.Is(err, domain.ErrWorktreeNotPrepared):
		status = http.StatusNotFound
		code = "WORKSPACE_NOT_READY"
	case errors.Is(err, domain.ErrExecNotFound):
		status = http.StatusNotFound
		code = "EXEC_NOT_FOUND"
	case errors.Is(err, domain.ErrVersionConflict), errors.Is(err, domain.ErrGitNoChanges), errors.Is(err, domain.ErrMergeConflict):
		status = http.StatusConflict
		code = "CONFLICT"
	}
	writeJSON(w, status, domain.APIError{
		Code:    code,
		Message: err.Error(),
	})
}
