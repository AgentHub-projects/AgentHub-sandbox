package filesystem

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agenthub-sandbox/internal/domain"
	"agenthub-sandbox/internal/watcher"
	"agenthub-sandbox/internal/worktree"
)

func TestServiceWriteReadAndWatch(t *testing.T) {
	root := t.TempDir()
	registry := worktree.NewRegistry()
	hub := watcher.NewHub()
	registry.Upsert(&worktree.State{
		AgentID:       "leader",
		BranchName:    "agent/leader",
		RootPath:      root,
		HeadSHA:       "head",
		ActiveExecIDs: map[string]struct{}{},
	})

	service, err := NewService(registry, hub)
	if err != nil {
		t.Fatalf("create service: %v", err)
	}
	defer service.Close()

	if err := service.SyncAgent("leader"); err != nil {
		t.Fatalf("sync agent: %v", err)
	}

	events, unsubscribe := hub.Subscribe("leader", "sub-1")
	defer unsubscribe()
	hub.SetPaths("sub-1", []string{"."})

	writeResult, err := service.Write("leader", "src/main.go", "package main\n", "", true, "ui")
	if err != nil {
		t.Fatalf("write file: %v", err)
	}
	if writeResult.Path != "src/main.go" {
		t.Fatalf("unexpected write path: %s", writeResult.Path)
	}

	readResult, err := service.Read("leader", "src/main.go", 0, 0)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if readResult.Content != "package main\n" {
		t.Fatalf("unexpected content: %q", readResult.Content)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case event := <-events:
			if event.Path == "src/main.go" && event.Actor == "ui" && event.ChangeType == "write" {
				goto gotWriteEvent
			}
		case <-deadline:
			t.Fatalf("timed out waiting for write watcher event")
		}
	}

gotWriteEvent:

	if _, err := service.Write("leader", "src/main.go", "package sandbox\n", "wrong-version", false, "ui"); !errors.Is(err, domain.ErrVersionConflict) {
		t.Fatalf("expected version conflict, got %v", err)
	}
}

func TestServiceLazilyEnsuresAgent(t *testing.T) {
	root := t.TempDir()
	registry := worktree.NewRegistry()
	hub := watcher.NewHub()

	service, err := NewService(registry, hub)
	if err != nil {
		t.Fatalf("create service: %v", err)
	}
	defer service.Close()

	ensured := 0
	service.SetEnsureAgent(func(agentID string) error {
		ensured++
		registry.Upsert(&worktree.State{
			AgentID:       agentID,
			BranchName:    "agent/" + agentID,
			RootPath:      root,
			HeadSHA:       "head",
			ActiveExecIDs: map[string]struct{}{},
		})
		return service.SyncAgent(agentID)
	})

	if _, err := service.Write("leader", "lazy.txt", "hello\n", "", true, "ui"); err != nil {
		t.Fatalf("write should lazily ensure agent: %v", err)
	}
	if ensured != 1 {
		t.Fatalf("expected one lazy ensure, got %d", ensured)
	}
	data, err := os.ReadFile(filepath.Join(root, "lazy.txt"))
	if err != nil {
		t.Fatalf("read lazy file: %v", err)
	}
	if string(data) != "hello\n" {
		t.Fatalf("unexpected lazy file content: %q", string(data))
	}

	if _, err := service.Read("leader", "lazy.txt", 0, 0); err != nil {
		t.Fatalf("read should use existing agent: %v", err)
	}
	if ensured != 1 {
		t.Fatalf("expected no second ensure, got %d", ensured)
	}
}
