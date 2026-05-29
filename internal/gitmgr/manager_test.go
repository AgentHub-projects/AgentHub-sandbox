package gitmgr

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"agenthub-sandbox/internal/worktree"
)

func TestManagerPrepareMergeAndPromote(t *testing.T) {
	repoRoot := t.TempDir()
	worktreeRoot := t.TempDir()

	runGit(t, repoRoot, "init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(repoRoot, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write initial file: %v", err)
	}
	runGit(t, repoRoot, "add", "README.md")
	runGitWithEnv(t, repoRoot, map[string]string{
		"GIT_AUTHOR_NAME":     "Test",
		"GIT_AUTHOR_EMAIL":    "test@example.com",
		"GIT_COMMITTER_NAME":  "Test",
		"GIT_COMMITTER_EMAIL": "test@example.com",
	}, "commit", "-m", "initial commit")

	registry := worktree.NewRegistry()
	manager := NewManager(repoRoot, worktreeRoot, registry, func(string, string, string, string) {})

	if _, err := manager.Prepare("leader", PrepareRequest{FromRef: "main", Reset: true}); err != nil {
		t.Fatalf("prepare leader: %v", err)
	}
	worker, err := manager.Prepare("worker-1", PrepareRequest{FromRef: "main", Reset: true})
	if err != nil {
		t.Fatalf("prepare worker: %v", err)
	}

	featurePath := filepath.Join(worker.RootPath, "feature.txt")
	if err := os.WriteFile(featurePath, []byte("worker change\n"), 0o644); err != nil {
		t.Fatalf("write worker file: %v", err)
	}
	if _, _, err := manager.Commit("worker-1", CommitRequest{Message: "add feature"}); err != nil {
		t.Fatalf("commit worker change: %v", err)
	}

	mergeResult, err := manager.Merge("leader", MergeRequest{SourceAgentID: "worker-1"})
	if err != nil {
		t.Fatalf("merge into leader: %v", err)
	}
	if mergeResult.Status != "merged" {
		t.Fatalf("expected merged status, got %+v", mergeResult)
	}

	promoteResult, err := manager.Promote("leader", PromoteRequest{TargetBranch: "main"})
	if err != nil {
		t.Fatalf("promote leader: %v", err)
	}
	if promoteResult.Status != "merged" {
		t.Fatalf("expected merged promote status, got %+v", promoteResult)
	}

	data, err := os.ReadFile(filepath.Join(repoRoot, "feature.txt"))
	if err != nil {
		t.Fatalf("read promoted file: %v", err)
	}
	if strings.ReplaceAll(string(data), "\r\n", "\n") != "worker change\n" {
		t.Fatalf("unexpected promoted content: %q", string(data))
	}
}

func TestManagerCompleteCommitsDirtyWorktree(t *testing.T) {
	repoRoot := t.TempDir()
	worktreeRoot := t.TempDir()

	runGit(t, repoRoot, "init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(repoRoot, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write initial file: %v", err)
	}
	runGit(t, repoRoot, "add", "README.md")
	runGitWithEnv(t, repoRoot, gitIdentityEnv("Test", "test@example.com"), "commit", "-m", "initial commit")

	registry := worktree.NewRegistry()
	manager := NewManager(repoRoot, worktreeRoot, registry, func(string, string, string, string) {})

	worker, err := manager.Prepare("worker-1", PrepareRequest{FromRef: "main", Reset: true})
	if err != nil {
		t.Fatalf("prepare worker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worker.RootPath, "result.txt"), []byte("done\n"), 0o644); err != nil {
		t.Fatalf("write worker file: %v", err)
	}

	result, err := manager.Complete("worker-1", CompleteRequest{Message: "agent(worker-1): complete session work"})
	if err != nil {
		t.Fatalf("complete worker: %v", err)
	}
	if result.Status != "committed" || result.CommitSHA == "" {
		t.Fatalf("expected committed completion result, got %+v", result)
	}
	status, err := manager.Status("worker-1")
	if err != nil {
		t.Fatalf("status worker: %v", err)
	}
	if len(status.Staged)+len(status.Unstaged)+len(status.Untracked)+len(status.Conflicted) != 0 {
		t.Fatalf("expected clean worktree after complete, got %+v", status)
	}

	clean, err := manager.Complete("worker-1", CompleteRequest{})
	if err != nil {
		t.Fatalf("complete clean worker: %v", err)
	}
	if clean.Status != "clean" {
		t.Fatalf("expected clean completion result, got %+v", clean)
	}
}

func TestManagerPrepareInitializesEmptyRepoRoot(t *testing.T) {
	repoRoot := t.TempDir()
	worktreeRoot := t.TempDir()

	registry := worktree.NewRegistry()
	manager := NewManager(repoRoot, worktreeRoot, registry, func(string, string, string, string) {})

	info, err := manager.Prepare("leader", PrepareRequest{Reset: true})
	if err != nil {
		t.Fatalf("prepare leader: %v", err)
	}

	if info.BranchName != "agent/leader" {
		t.Fatalf("unexpected branch name: %s", info.BranchName)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, ".git")); err != nil {
		t.Fatalf("expected repo root to be initialized: %v", err)
	}
	if head := gitOutput(t, repoRoot, "rev-parse", "--abbrev-ref", "HEAD"); strings.TrimSpace(head) != "main" {
		t.Fatalf("expected main branch, got %q", head)
	}
}

func TestManagerPrepareInitializesRepoRootWithSnapshot(t *testing.T) {
	repoRoot := t.TempDir()
	worktreeRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write initial file: %v", err)
	}

	registry := worktree.NewRegistry()
	manager := NewManager(repoRoot, worktreeRoot, registry, func(string, string, string, string) {})

	info, err := manager.Prepare("worker-1", PrepareRequest{Reset: true})
	if err != nil {
		t.Fatalf("prepare worker: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(info.RootPath, "README.md"))
	if err != nil {
		t.Fatalf("read snapshot file from worktree: %v", err)
	}
	if strings.ReplaceAll(string(data), "\r\n", "\n") != "hello\n" {
		t.Fatalf("unexpected snapshot content: %q", string(data))
	}
	if status := gitOutput(t, repoRoot, "status", "--short"); strings.TrimSpace(status) != "" {
		t.Fatalf("expected clean initialized repo, got status %q", status)
	}
}

func TestManagerStatusLazilyPreparesWorktree(t *testing.T) {
	repoRoot := t.TempDir()
	worktreeRoot := t.TempDir()

	registry := worktree.NewRegistry()
	manager := NewManager(repoRoot, worktreeRoot, registry, func(string, string, string, string) {})

	status, err := manager.Status("leader")
	if err != nil {
		t.Fatalf("status should lazily prepare worktree: %v", err)
	}
	if status.BranchName != "agent/leader" {
		t.Fatalf("unexpected branch name: %s", status.BranchName)
	}
	if _, ok := registry.Get("leader"); !ok {
		t.Fatalf("expected leader to be registered")
	}
	if _, err := os.Stat(filepath.Join(worktreeRoot, "leader", ".git")); err != nil {
		t.Fatalf("expected leader worktree to exist: %v", err)
	}
}

func TestManagerSyncMergesMainIntoAgentWorktree(t *testing.T) {
	repoRoot := t.TempDir()
	worktreeRoot := t.TempDir()

	runGit(t, repoRoot, "init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(repoRoot, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write initial file: %v", err)
	}
	runGit(t, repoRoot, "add", "README.md")
	runGitWithEnv(t, repoRoot, map[string]string{
		"GIT_AUTHOR_NAME":     "Test",
		"GIT_AUTHOR_EMAIL":    "test@example.com",
		"GIT_COMMITTER_NAME":  "Test",
		"GIT_COMMITTER_EMAIL": "test@example.com",
	}, "commit", "-m", "initial commit")

	registry := worktree.NewRegistry()
	manager := NewManager(repoRoot, worktreeRoot, registry, func(string, string, string, string) {})
	worker, err := manager.Prepare("worker-1", PrepareRequest{FromRef: "main", Reset: true})
	if err != nil {
		t.Fatalf("prepare worker: %v", err)
	}

	if err := os.WriteFile(filepath.Join(repoRoot, "main.txt"), []byte("main change\n"), 0o644); err != nil {
		t.Fatalf("write main file: %v", err)
	}
	runGit(t, repoRoot, "add", "main.txt")
	runGitWithEnv(t, repoRoot, map[string]string{
		"GIT_AUTHOR_NAME":     "Test",
		"GIT_AUTHOR_EMAIL":    "test@example.com",
		"GIT_COMMITTER_NAME":  "Test",
		"GIT_COMMITTER_EMAIL": "test@example.com",
	}, "commit", "-m", "main update")

	result, err := manager.Sync("worker-1", SyncRequest{})
	if err != nil {
		t.Fatalf("sync worker: %v", err)
	}
	if result.Status != "synced" {
		t.Fatalf("expected synced status, got %+v", result)
	}
	if result.SourceRef != "main" || result.TargetBranch != "agent/worker-1" || result.HeadSHA == "" {
		t.Fatalf("unexpected sync result: %+v", result)
	}
	data, err := os.ReadFile(filepath.Join(worker.RootPath, "main.txt"))
	if err != nil {
		t.Fatalf("read synced file: %v", err)
	}
	if strings.ReplaceAll(string(data), "\r\n", "\n") != "main change\n" {
		t.Fatalf("unexpected synced content: %q", string(data))
	}
}

func TestManagerSyncRefusesDirtyWorktree(t *testing.T) {
	repoRoot := t.TempDir()
	worktreeRoot := t.TempDir()

	registry := worktree.NewRegistry()
	manager := NewManager(repoRoot, worktreeRoot, registry, func(string, string, string, string) {})
	worker, err := manager.Prepare("worker-1", PrepareRequest{Reset: true})
	if err != nil {
		t.Fatalf("prepare worker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worker.RootPath, "dirty.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}

	result, err := manager.Sync("worker-1", SyncRequest{})
	if err != nil {
		t.Fatalf("sync dirty worker: %v", err)
	}
	if result.Status != "dirty" {
		t.Fatalf("expected dirty status, got %+v", result)
	}
	if len(result.Dirty) != 1 || result.Dirty[0] != "dirty.txt" {
		t.Fatalf("unexpected dirty paths: %+v", result.Dirty)
	}
}

func TestManagerRestoreWorktreesFromGitMetadata(t *testing.T) {
	repoRoot := t.TempDir()
	worktreeRoot := t.TempDir()

	registry := worktree.NewRegistry()
	manager := NewManager(repoRoot, worktreeRoot, registry, func(string, string, string, string) {})
	leader, err := manager.Prepare("leader", PrepareRequest{Reset: true})
	if err != nil {
		t.Fatalf("prepare leader: %v", err)
	}
	if _, err := manager.Prepare("worker-1", PrepareRequest{Reset: true}); err != nil {
		t.Fatalf("prepare worker: %v", err)
	}

	restoredRegistry := worktree.NewRegistry()
	restoredManager := NewManager(repoRoot, worktreeRoot, restoredRegistry, func(string, string, string, string) {})
	restored, err := restoredManager.RestoreWorktrees()
	if err != nil {
		t.Fatalf("restore worktrees: %v", err)
	}
	if len(restored) != 2 {
		t.Fatalf("expected two restored agents, got %+v", restored)
	}

	got, ok := restoredRegistry.Get("leader")
	if !ok {
		t.Fatalf("expected leader to be restored")
	}
	if got.BranchName != "agent/leader" || got.RootPath != leader.RootPath || got.HeadSHA != leader.HeadSHA {
		t.Fatalf("unexpected restored leader: %+v, original %+v", got, leader)
	}
	if len(got.ActiveExecIDs) != 0 {
		t.Fatalf("expected restored exec ids to be empty, got %+v", got.ActiveExecIDs)
	}
}

func runGit(t *testing.T, cwd string, args ...string) {
	t.Helper()
	runGitWithEnv(t, cwd, nil, args...)
}

func runGitWithEnv(t *testing.T, cwd string, env map[string]string, args ...string) {
	t.Helper()
	_ = gitOutputWithEnv(t, cwd, env, args...)
}

func gitOutput(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	return gitOutputWithEnv(t, cwd, nil, args...)
}

func gitOutputWithEnv(t *testing.T, cwd string, env map[string]string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	cmd.Env = os.Environ()
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, string(output))
	}
	return string(output)
}
