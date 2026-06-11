package socketio

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"

	"github.com/google/uuid"
	engineTransports "github.com/zishang520/socket.io/servers/engine/v3/transports"
	socket "github.com/zishang520/socket.io/servers/socket/v3"
	"github.com/zishang520/socket.io/v3/pkg/types"

	"agenthub-sandbox/internal/domain"
	"agenthub-sandbox/internal/executor"
	"agenthub-sandbox/internal/filesystem"
	"agenthub-sandbox/internal/gitmgr"
	"agenthub-sandbox/internal/watcher"
)

type Server struct {
	fsService *filesystem.Service
	exec      *executor.Manager
	git       *gitmgr.Manager
	fsIO      *socket.Server
	agentsIO  *socket.Server

	mainCommitMu      sync.RWMutex
	mainCommitClients map[*socket.Socket]struct{}
}

// Server 把 /filesystem 和 /agents 两套 Socket.IO 服务封装在一起。
func New(fsService *filesystem.Service, execManager *executor.Manager, gitManager *gitmgr.Manager) *Server {
	return &Server{
		fsService:         fsService,
		exec:              execManager,
		git:               gitManager,
		fsIO:              newSocketServer("/filesystem/socket.io"),
		agentsIO:          newSocketServer("/agents/socket.io"),
		mainCommitClients: make(map[*socket.Socket]struct{}),
	}
}

func (s *Server) Register(mux *http.ServeMux) {
	// 两个 socket path 拆开挂，避免和 gateway 自己的 /socket.io 冲突。
	s.bindFilesystem()
	s.bindAgents()

	fsHandler := s.fsIO.ServeHandler(nil)
	agentsHandler := s.agentsIO.ServeHandler(nil)
	mux.Handle("/filesystem/socket.io", fsHandler)
	mux.Handle("/filesystem/socket.io/", fsHandler)
	mux.Handle("/agents/socket.io", agentsHandler)
	mux.Handle("/agents/socket.io/", agentsHandler)
}

func (s *Server) Close() error {
	if s.fsIO != nil {
		s.fsIO.Close(nil)
	}
	if s.agentsIO != nil {
		s.agentsIO.Close(nil)
	}
	return nil
}

// bindFilesystem 处理文件读写、目录浏览和文件变更订阅。
func (s *Server) bindFilesystem() {
	nsp := s.fsIO.Of("/", nil)
	_ = nsp.On("connection", func(args ...any) {
		client := args[0].(*socket.Socket)
		workspaceID := domain.MainWorkspaceID
		if _, err := s.fsService.Info(workspaceID); err != nil {
			_ = client.Emit("connect_error", workspaceEnvelope("", nil, err))
			client.Disconnect(true)
			return
		}
		s.addMainCommitClient(client)

		subID := uuid.NewString()
		events, unsubscribe := s.fsServiceWatcher(workspaceID, subID)
		s.fsServiceWatcherPaths(subID, []string{"."})
		go func() {
			for event := range events {
				_ = client.Emit("fs:changed", filesystemChangePayload(event))
			}
		}()
		_ = client.On("disconnect", func(...any) {
			unsubscribe()
			s.removeMainCommitClient(client)
		})

		registerJSONHandler(client, "fs:list", func(request requestEnvelope) any {
			var body struct {
				RequestID string `json:"requestId"`
				Path      string `json:"path"`
				Depth     int    `json:"depth"`
			}
			if err := decodeBody(request.Raw, &body); err != nil {
				return workspaceEnvelope(request.RequestID, nil, err)
			}
			items, err := s.fsService.List(workspaceID, body.Path, body.Depth)
			if err != nil {
				return workspaceEnvelope(request.RequestID, nil, err)
			}
			return workspaceEnvelope(request.RequestID, map[string]any{"entries": items}, nil)
		})
		registerJSONHandler(client, "fs:read", func(request requestEnvelope) any {
			var body struct {
				RequestID string `json:"requestId"`
				Path      string `json:"path"`
				LineStart int    `json:"lineStart"`
				LineEnd   int    `json:"lineEnd"`
			}
			if err := decodeBody(request.Raw, &body); err != nil {
				return workspaceEnvelope(request.RequestID, nil, err)
			}
			read, err := s.fsService.Read(workspaceID, body.Path, body.LineStart, body.LineEnd)
			if err != nil {
				return workspaceEnvelope(request.RequestID, nil, err)
			}
			return workspaceEnvelope(request.RequestID, read, nil)
		})
		registerJSONHandler(client, "fs:update", func(request requestEnvelope) any {
			var body struct {
				RequestID       string                `json:"requestId"`
				Path            string                `json:"path"`
				ExpectedVersion string                `json:"expectedVersion"`
				Edits           []filesystem.TextEdit `json:"edits"`
				CreateDirs      bool                  `json:"createDirs"`
			}
			if err := decodeBody(request.Raw, &body); err != nil {
				return workspaceEnvelope(request.RequestID, nil, err)
			}
			written, err := s.fsService.ApplyEdits(workspaceID, body.Path, body.ExpectedVersion, body.Edits, body.CreateDirs, "ui")
			if err != nil {
				return workspaceEnvelope(request.RequestID, nil, err)
			}
			branchName, commitSHA, err := s.git.Commit(workspaceID, gitmgr.CommitRequest{
				Message:     "workspace: update " + written.Path,
				AuthorName:  "AgentHub User",
				AuthorEmail: "user@agenthub.local",
			})
			if err != nil && !errors.Is(err, domain.ErrGitNoChanges) {
				return workspaceEnvelope(request.RequestID, nil, err)
			}
			if branchName == "" {
				branchName = "main"
			}
			return workspaceEnvelope(request.RequestID, map[string]any{
				"path":       written.Path,
				"size":       written.Size,
				"mtime":      written.Mtime,
				"version":    written.Version,
				"branchName": branchName,
				"commitSha":  commitSHA,
			}, nil)
		})
	})
}

func (s *Server) EmitMainCommitted(event domain.MainCommitEvent) {
	s.mainCommitMu.RLock()
	clients := make([]*socket.Socket, 0, len(s.mainCommitClients))
	for client := range s.mainCommitClients {
		clients = append(clients, client)
	}
	s.mainCommitMu.RUnlock()

	for _, client := range clients {
		_ = client.Emit("main:committed", event)
	}
}

func (s *Server) addMainCommitClient(client *socket.Socket) {
	s.mainCommitMu.Lock()
	defer s.mainCommitMu.Unlock()
	s.mainCommitClients[client] = struct{}{}
}

func (s *Server) removeMainCommitClient(client *socket.Socket) {
	s.mainCommitMu.Lock()
	defer s.mainCommitMu.Unlock()
	delete(s.mainCommitClients, client)
}

// bindAgents 处理命令执行流和 agent 侧文件访问。
func (s *Server) bindAgents() {
	nsp := s.agentsIO.Of("/", nil)
	_ = nsp.On("connection", func(args ...any) {
		client := args[0].(*socket.Socket)
		agentID, err := socketAgentID(client)
		if err != nil {
			_ = client.Emit("connect_error", envelope("", nil, err))
			client.Disconnect(true)
			return
		}
		if _, err := s.fsService.Info(agentID); err != nil {
			_ = client.Emit("connect_error", envelope("", nil, err))
			client.Disconnect(true)
			return
		}

		registerJSONHandler(client, "agent:info", func(request requestEnvelope) any {
			info, err := s.fsService.Info(agentID)
			if err != nil {
				return envelope(request.RequestID, nil, err)
			}
			return envelope(request.RequestID, map[string]any{
				"agentId":    info.AgentID,
				"rootPath":   info.RootPath,
				"branchName": info.BranchName,
				"headSha":    info.HeadSHA,
			}, nil)
		})

		registerJSONHandler(client, "file:read", func(request requestEnvelope) any {
			var body struct {
				RequestID string `json:"requestId"`
				Path      string `json:"path"`
				LineStart int    `json:"lineStart"`
				LineEnd   int    `json:"lineEnd"`
			}
			if err := decodeBody(request.Raw, &body); err != nil {
				return envelope(request.RequestID, nil, err)
			}
			read, err := s.fsService.Read(agentID, body.Path, body.LineStart, body.LineEnd)
			if err != nil {
				return envelope(request.RequestID, nil, err)
			}
			return envelope(request.RequestID, read, nil)
		})
		registerJSONHandler(client, "file:write", func(request requestEnvelope) any {
			var body struct {
				RequestID       string `json:"requestId"`
				Path            string `json:"path"`
				Content         string `json:"content"`
				ExpectedVersion string `json:"expectedVersion"`
			}
			if err := decodeBody(request.Raw, &body); err != nil {
				return envelope(request.RequestID, nil, err)
			}
			written, err := s.fsService.Write(agentID, body.Path, body.Content, body.ExpectedVersion, true, "agent")
			if err != nil {
				return envelope(request.RequestID, nil, err)
			}
			return envelope(request.RequestID, written, nil)
		})
		registerJSONHandler(client, "exec:start", func(request requestEnvelope) any {
			var body executor.StartRequest
			if err := decodeBody(request.Raw, &body); err != nil {
				return envelope(request.RequestID, nil, err)
			}
			var execID string
			started, err := s.exec.Start(agentID, body, executor.Callbacks{
				Stdout: func(chunk string) {
					_ = client.Emit("exec:stdout", map[string]any{
						"execId": execID,
						"chunk":  chunk,
					})
				},
				Stderr: func(chunk string) {
					_ = client.Emit("exec:stderr", map[string]any{
						"execId": execID,
						"chunk":  chunk,
					})
				},
				Exit: func(result executor.RunResult) {
					_ = client.Emit("exec:exit", map[string]any{
						"execId":     execID,
						"exitCode":   result.ExitCode,
						"finishedAt": result.FinishedAt,
						"timedOut":   result.TimedOut,
					})
				},
				Error: func(err error) {
					_ = client.Emit("exec:error", map[string]any{
						"execId":  execID,
						"code":    "EXEC_ERROR",
						"message": err.Error(),
					})
				},
			})
			if err != nil {
				return envelope(request.RequestID, nil, err)
			}
			execID = started.ExecID
			return envelope(request.RequestID, started, nil)
		})
		registerJSONHandler(client, "exec:stdin", func(request requestEnvelope) any {
			var body struct {
				RequestID string `json:"requestId"`
				ExecID    string `json:"execId"`
				Chunk     string `json:"chunk"`
			}
			if err := decodeBody(request.Raw, &body); err != nil {
				return envelope(request.RequestID, nil, err)
			}
			if err := s.exec.WriteStdin(body.ExecID, body.Chunk); err != nil {
				return envelope(request.RequestID, nil, err)
			}
			return envelope(request.RequestID, map[string]any{"accepted": true}, nil)
		})
		registerJSONHandler(client, "exec:kill", func(request requestEnvelope) any {
			var body struct {
				RequestID string `json:"requestId"`
				ExecID    string `json:"execId"`
				Signal    string `json:"signal"`
			}
			if err := decodeBody(request.Raw, &body); err != nil {
				return envelope(request.RequestID, nil, err)
			}
			if err := s.exec.Kill(body.ExecID, body.Signal); err != nil {
				return envelope(request.RequestID, nil, err)
			}
			return envelope(request.RequestID, map[string]any{"accepted": true}, nil)
		})
	})
}

type requestEnvelope struct {
	RequestID string
	Raw       map[string]any
}

func newSocketServer(path string) *socket.Server {
	opts := socket.DefaultServerOptions()
	opts.SetPath(path)
	opts.SetCors(&types.Cors{
		Origin:      "*",
		Credentials: true,
	})
	// 只开 websocket transport，符合协议要求，也能减少和 gateway 的歧义。
	opts.SetTransports(types.NewSet(engineTransports.Transports()[engineTransports.WEBSOCKET]))
	return socket.NewServer(nil, opts)
}

// registerJSONHandler 统一处理 requestId、ack 和无 ack 时的 fallback response。
func registerJSONHandler(client *socket.Socket, event string, handler func(request requestEnvelope) any) {
	_ = client.On(event, func(args ...any) {
		request := parseRequest(args)
		payload := handler(request)

		if ack := lastAck(args); ack != nil {
			ack([]any{payload}, nil)
			return
		}
		_ = client.Emit(event+":response", payload)
	})
}

func parseRequest(args []any) requestEnvelope {
	envelope := requestEnvelope{
		Raw: make(map[string]any),
	}
	if len(args) == 0 {
		return envelope
	}
	if body, ok := args[0].(map[string]any); ok {
		envelope.Raw = body
		if requestID, _ := body["requestId"].(string); requestID != "" {
			envelope.RequestID = requestID
		}
	}
	return envelope
}

func decodeBody(raw map[string]any, target any) error {
	data, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

func lastAck(args []any) socket.Ack {
	if len(args) == 0 {
		return nil
	}
	ack, _ := args[len(args)-1].(socket.Ack)
	return ack
}

func filesystemChangePayload(event watcher.Event) map[string]any {
	return map[string]any{
		"path":       event.Path,
		"changeType": event.ChangeType,
		"mtime":      event.Mtime,
		"version":    event.Version,
		"actor":      event.Actor,
	}
}

func socketAgentID(client *socket.Socket) (string, error) {
	handshake := client.Handshake()
	if handshake == nil {
		return "", domain.ErrAgentIDRequired
	}
	if agentID, ok := handshake.Auth["agentId"].(string); ok && agentID != "" {
		return agentID, nil
	}
	if queryValues := handshake.Query.Query(); queryValues != nil {
		if agentID := queryValues.Get("agentId"); agentID != "" {
			return agentID, nil
		}
	}
	return "", domain.ErrAgentIDRequired
}

// envelope 保持所有 socket 事件的返回格式一致。
func envelope(requestID string, data any, err error) domain.ResponseEnvelope {
	return envelopeWithErrorCode(requestID, data, err, errorCode)
}

func workspaceEnvelope(requestID string, data any, err error) domain.ResponseEnvelope {
	return envelopeWithErrorCode(requestID, data, err, workspaceErrorCode)
}

func envelopeWithErrorCode(requestID string, data any, err error, codeFor func(error) string) domain.ResponseEnvelope {
	if err != nil {
		return domain.ResponseEnvelope{
			RequestID: requestID,
			OK:        false,
			Error: &domain.APIError{
				Code:    codeFor(err),
				Message: err.Error(),
			},
		}
	}
	return domain.ResponseEnvelope{
		RequestID: requestID,
		OK:        true,
		Data:      data,
	}
}

func workspaceErrorCode(err error) string {
	if errors.Is(err, domain.ErrWorktreeNotPrepared) {
		return "WORKSPACE_NOT_READY"
	}
	return errorCode(err)
}

func errorCode(err error) string {
	switch {
	case errors.Is(err, domain.ErrAgentIDRequired):
		return "AGENT_ID_REQUIRED"
	case errors.Is(err, domain.ErrWorktreeNotPrepared):
		return "WORKTREE_NOT_PREPARED"
	case errors.Is(err, domain.ErrInvalidPath), errors.Is(err, domain.ErrPathEscapesWorktree):
		return "INVALID_PATH"
	case errors.Is(err, domain.ErrInvalidEdit):
		return "INVALID_EDIT"
	case errors.Is(err, domain.ErrVersionConflict):
		return "VERSION_CONFLICT"
	case errors.Is(err, domain.ErrExecNotFound):
		return "EXEC_NOT_FOUND"
	default:
		return "INTERNAL_ERROR"
	}
}

func (s *Server) fsServiceWatcher(agentID, subID string) (<-chan watcher.Event, func()) {
	return s.fsServiceWatcherHub().Subscribe(agentID, subID)
}

func (s *Server) fsServiceWatcherPaths(subID string, paths []string) []string {
	return s.fsServiceWatcherHub().SetPaths(subID, paths)
}

func (s *Server) fsServiceWatcherHub() *watcher.Hub {
	return s.fsService.Hub()
}
