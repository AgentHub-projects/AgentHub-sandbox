package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agenthub-sandbox/internal/domain"
	"agenthub-sandbox/internal/filesystem"
	"agenthub-sandbox/internal/gitmgr"
	"agenthub-sandbox/internal/watcher"
	"agenthub-sandbox/internal/worktree"
)

func TestRouterMainGitEndpoints(t *testing.T) {
	repoRoot := t.TempDir()
	worktreeRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write initial file: %v", err)
	}

	manager := gitmgr.NewManager(repoRoot, worktreeRoot, worktree.NewRegistry(), func(string, string, string, string) {})
	if _, err := manager.EnsureMainWorkspace(); err != nil {
		t.Fatalf("ensure main workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "README.md"), []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatalf("modify file: %v", err)
	}
	_, commitSHA, err := manager.Commit(domain.MainWorkspaceID, gitmgr.CommitRequest{Message: "workspace: update README.md"})
	if err != nil {
		t.Fatalf("commit update: %v", err)
	}

	mux := http.NewServeMux()
	New(nil, nil, manager).Register(mux)

	var commits domain.MainCommitList
	getJSON(t, mux, "/filesystem/git/main/commits?limit=1", http.StatusOK, &commits)
	if len(commits.Items) != 1 || commits.Items[0].CommitSHA != commitSHA {
		t.Fatalf("unexpected commits response: %+v", commits)
	}
	getStatus(t, mux, "/git/main/commits?limit=1", http.StatusNotFound)

	var files domain.MainCommitDiffFiles
	getJSON(t, mux, "/filesystem/git/main/commits/"+commitSHA+"/diff/files", http.StatusOK, &files)
	if len(files.Files) != 1 || files.Files[0].Path != "README.md" || files.Files[0].Status != "modified" {
		t.Fatalf("unexpected diff files response: %+v", files)
	}

	var file domain.MainCommitDiffFile
	getJSON(t, mux, "/filesystem/git/main/commits/"+commitSHA+"/diff/file?path=README.md", http.StatusOK, &file)
	if !file.BaseFile.Exists || file.BaseFile.Content == nil || *file.BaseFile.Content != "hello\n" {
		t.Fatalf("unexpected file diff response: %+v", file)
	}
	if !strings.Contains(file.Patch, "+world") {
		t.Fatalf("expected patch to contain added line, got %q", file.Patch)
	}

	getStatus(t, mux, "/filesystem/git/agents/worker-1/diff", http.StatusNotFound)

	var apiErr domain.APIError
	getJSON(t, mux, "/filesystem/git/main/commits/not-a-commit/diff/files", http.StatusBadRequest, &apiErr)
	if apiErr.Code != "INVALID_COMMIT" {
		t.Fatalf("unexpected error response: %+v", apiErr)
	}
}

func TestRouterImageManifestIsVisibleThroughFilesystemRead(t *testing.T) {
	root := t.TempDir()
	mainRoot := t.TempDir()
	registry := worktree.NewRegistry()
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
	if err := os.MkdirAll(filepath.Join(root, "assets", "generated"), 0o755); err != nil {
		t.Fatalf("create image dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "assets", "generated", "test.png"), []byte{0x89, 0x50, 0x4e, 0x47}, 0o644); err != nil {
		t.Fatalf("write image: %v", err)
	}
	fsService, err := filesystem.NewService(registry, watcher.NewHub())
	if err != nil {
		t.Fatalf("create filesystem service: %v", err)
	}
	defer fsService.Close()

	mux := http.NewServeMux()
	New(fsService, nil, nil).Register(mux)

	body := []byte(`{
		"path": "assets/generated/test.png",
		"previewUrl": "https://cdn.example.com/test.png",
		"mimeType": "image/png",
		"sizeBytes": 4
	}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/git/agents/leader/images/manifest", bytes.NewReader(body))
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST manifest status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/read/assets%2Fgenerated%2Ftest.png", nil)
	req.Header.Set("X-AgentHub-Agent-Id", "leader")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET filesystem read status=%d body=%s", rec.Code, rec.Body.String())
	}
	var read filesystem.ReadResult
	if err := json.NewDecoder(rec.Body).Decode(&read); err != nil {
		t.Fatalf("decode read response: %v", err)
	}
	if read.Preview == nil || read.Preview.PreviewURL != "https://cdn.example.com/test.png" {
		t.Fatalf("unexpected preview: %+v", read.Preview)
	}
	if read.Content != "" {
		t.Fatalf("image read should not return binary content, got %q", read.Content)
	}

	getStatus(t, mux, "/git/agents/leader/download/assets%2Fgenerated%2Ftest.png", http.StatusNotFound)
	getStatus(t, mux, "/filesystem/read/assets%2Fgenerated%2Ftest.png", http.StatusNotFound)
}

func TestRouterWriteStaysInsideAgentWorkspace(t *testing.T) {
	root := t.TempDir()
	mainRoot := t.TempDir()
	registry := worktree.NewRegistry()
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
	fsService, err := filesystem.NewService(registry, watcher.NewHub())
	if err != nil {
		t.Fatalf("create filesystem service: %v", err)
	}
	defer fsService.Close()

	mux := http.NewServeMux()
	New(fsService, nil, nil).Register(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/write/docs%2Fhello.txt", bytes.NewReader([]byte(`{"content":"hello"}`)))
	req.Header.Set("X-AgentHub-Agent-Id", "leader")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST write status=%d body=%s", rec.Code, rec.Body.String())
	}
	content, err := os.ReadFile(filepath.Join(root, "docs", "hello.txt"))
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(content) != "hello" {
		t.Fatalf("unexpected written content: %q", content)
	}
	if _, err := os.Stat(filepath.Join(mainRoot, "docs", "hello.txt")); !os.IsNotExist(err) {
		t.Fatalf("write should not touch main workspace, stat err=%v", err)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/write/%2Fworkspace%2Fhello.txt", bytes.NewReader([]byte(`{"content":"bad"}`)))
	req.Header.Set("X-AgentHub-Agent-Id", "leader")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST absolute write status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func getStatus(t *testing.T, mux http.Handler, target string, wantStatus int) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != wantStatus {
		t.Fatalf("GET %s status=%d body=%s", target, rec.Code, rec.Body.String())
	}
}

func getJSON(t *testing.T, mux http.Handler, target string, wantStatus int, out any) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != wantStatus {
		t.Fatalf("GET %s status=%d body=%s", target, rec.Code, rec.Body.String())
	}
	if err := json.NewDecoder(rec.Body).Decode(out); err != nil {
		t.Fatalf("decode %s: %v", target, err)
	}
}
