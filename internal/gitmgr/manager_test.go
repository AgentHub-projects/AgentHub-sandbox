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

func runGit(t *testing.T, cwd string, args ...string) {
	t.Helper()
	runGitWithEnv(t, cwd, nil, args...)
}

func runGitWithEnv(t *testing.T, cwd string, env map[string]string, args ...string) {
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
}
