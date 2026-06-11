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
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("create fake git metadata dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte("[core]\n"), 0o644); err != nil {
		t.Fatalf("write fake git config: %v", err)
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

	updateResult, err := service.ApplyEdits("leader", "src/main.go", readResult.Version, []TextEdit{
		{
			StartLine:   1,
			StartColumn: 9,
			EndLine:     1,
			EndColumn:   13,
			Text:        "sandbox",
		},
	}, false, "ui")
	if err != nil {
		t.Fatalf("apply edits: %v", err)
	}
	if updateResult.Path != "src/main.go" {
		t.Fatalf("unexpected update path: %s", updateResult.Path)
	}

	readResult, err = service.Read("leader", "src/main.go", 0, 0)
	if err != nil {
		t.Fatalf("read updated file: %v", err)
	}
	if readResult.Content != "package sandbox\n" {
		t.Fatalf("unexpected updated content: %q", readResult.Content)
	}
	entries, err := service.List("leader", ".", 1)
	if err != nil {
		t.Fatalf("list root: %v", err)
	}
	for _, entry := range entries {
		if entry.Path == ".git" {
			t.Fatalf("git metadata should not be listed: %+v", entries)
		}
	}
	if _, err := service.Read("leader", ".git/config", 0, 0); !errors.Is(err, domain.ErrInvalidPath) {
		t.Fatalf("expected .git read to be rejected, got %v", err)
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

	if _, err := service.ApplyEdits("leader", "src/main.go", "wrong-version", []TextEdit{
		{StartLine: 1, StartColumn: 1, EndLine: 1, EndColumn: 1, Text: "// "},
	}, false, "ui"); !errors.Is(err, domain.ErrVersionConflict) {
		t.Fatalf("expected version conflict, got %v", err)
	}

	if _, err := service.ApplyEdits("leader", "src/main.go", readResult.Version, []TextEdit{
		{StartLine: 99, StartColumn: 1, EndLine: 99, EndColumn: 1, Text: "nope"},
	}, false, "ui"); !errors.Is(err, domain.ErrInvalidEdit) {
		t.Fatalf("expected invalid edit, got %v", err)
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

func TestServiceImagePreviewManifest(t *testing.T) {
	root := t.TempDir()
	mainRoot := t.TempDir()
	registry := worktree.NewRegistry()
	hub := watcher.NewHub()
	registry.Upsert(&worktree.State{
		AgentID:       domain.MainWorkspaceID,
		BranchName:    "main",
		RootPath:      mainRoot,
		HeadSHA:       "main-head",
		ActiveExecIDs: map[string]struct{}{},
	})
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

	if err := os.MkdirAll(filepath.Join(root, "assets", "generated"), 0o755); err != nil {
		t.Fatalf("create image dir: %v", err)
	}
	imagePath := filepath.Join(root, "assets", "generated", "test.png")
	if err := os.WriteFile(imagePath, []byte{0x89, 0x50, 0x4e, 0x47}, 0o644); err != nil {
		t.Fatalf("write image: %v", err)
	}

	written, err := service.UpsertImagePreview("leader", domain.ImagePreview{
		Path:         "assets/generated/test.png",
		PreviewURL:   "https://cdn.example.com/test.png",
		OssURI:       "oss://bucket/key",
		OssObjectKey: "key",
		MimeType:     "image/png",
		SizeBytes:    4,
		Width:        1,
		Height:       1,
		SessionID:    "session-1",
	})
	if err != nil {
		t.Fatalf("upsert image preview: %v", err)
	}
	if written.Path != "assets/generated/test.png" || written.PreviewURL != "https://cdn.example.com/test.png" {
		t.Fatalf("unexpected written preview: %+v", written)
	}
	if _, err := os.Stat(filepath.Join(root, ".agenthub", "image-manifest.json")); err != nil {
		t.Fatalf("manifest not written: %v", err)
	}

	read, err := service.Read("leader", "assets/generated/test.png", 0, 0)
	if err != nil {
		t.Fatalf("read image: %v", err)
	}
	if read.Preview == nil || read.Preview.PreviewURL != "https://cdn.example.com/test.png" {
		t.Fatalf("read did not include preview: %+v", read.Preview)
	}

	entries, err := service.List("leader", "assets/generated", 1)
	if err != nil {
		t.Fatalf("list image dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Preview == nil || entries[0].Preview.PreviewURL != "https://cdn.example.com/test.png" {
		t.Fatalf("list did not include image preview: %+v", entries)
	}

	rootEntries, err := service.List("leader", ".", 1)
	if err != nil {
		t.Fatalf("list root: %v", err)
	}
	for _, entry := range rootEntries {
		if entry.Path == ".agenthub" {
			t.Fatalf("internal manifest dir should not be listed: %+v", rootEntries)
		}
	}
	if _, err := service.Read("leader", ".agenthub/image-manifest.json", 0, 0); !errors.Is(err, domain.ErrInvalidPath) {
		t.Fatalf("expected internal manifest read to be rejected, got %v", err)
	}
}
