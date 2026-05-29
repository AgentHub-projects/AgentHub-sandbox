package app

import (
	"net/http"
	"os"

	"agenthub-sandbox/internal/config"
	"agenthub-sandbox/internal/executor"
	"agenthub-sandbox/internal/filesystem"
	"agenthub-sandbox/internal/gitmgr"
	"agenthub-sandbox/internal/transport/httpapi"
	"agenthub-sandbox/internal/transport/socketio"
	"agenthub-sandbox/internal/watcher"
	"agenthub-sandbox/internal/worktree"
)

func New(cfg config.Config) (http.Handler, func(), error) {
	if err := os.MkdirAll(cfg.WorktreeRoot, 0o755); err != nil {
		return nil, func() {}, err
	}

	// 这里把单 session sandbox 需要的核心组件统一装配起来。
	registry := worktree.NewRegistry()
	hub := watcher.NewHub()

	fsService, err := filesystem.NewService(registry, hub)
	if err != nil {
		return nil, func() {}, err
	}
	execManager := executor.NewManager(registry)
	gitManager := gitmgr.NewManager(cfg.RepoRoot, cfg.WorktreeRoot, registry, fsService.NotifyChange)
	ensureAgent := func(agentID string) error {
		if _, err := gitManager.Ensure(agentID); err != nil {
			return err
		}
		return fsService.SyncAgent(agentID)
	}
	fsService.SetEnsureAgent(ensureAgent)
	execManager.SetEnsureAgent(ensureAgent)

	restoredAgents, err := gitManager.RestoreWorktrees()
	if err != nil {
		_ = fsService.Close()
		return nil, func() {}, err
	}
	for _, info := range restoredAgents {
		if err := fsService.SyncAgent(info.AgentID); err != nil {
			_ = fsService.Close()
			return nil, func() {}, err
		}
	}
	socketServer := socketio.New(fsService, execManager)
	httpRouter := httpapi.New(fsService, execManager, gitManager)

	mux := http.NewServeMux()
	httpRouter.Register(mux)
	socketServer.Register(mux)

	cleanup := func() {
		// 按相反顺序关闭，先停连接入口，再收后台资源。
		_ = socketServer.Close()
		_ = execManager.Close()
		_ = fsService.Close()
	}

	return mux, cleanup, nil
}
