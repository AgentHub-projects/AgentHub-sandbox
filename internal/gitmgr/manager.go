package gitmgr

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"agenthub-sandbox/internal/domain"
	"agenthub-sandbox/internal/security"
	"agenthub-sandbox/internal/worktree"
)

var agentIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

type PrepareRequest struct {
	FromRef    string `json:"fromRef"`
	BranchName string `json:"branchName"`
	Reset      bool   `json:"reset"`
}

type CommitRequest struct {
	Message     string `json:"message"`
	AuthorName  string `json:"authorName"`
	AuthorEmail string `json:"authorEmail"`
}

type MergeRequest struct {
	SourceAgentID string `json:"sourceAgentId"`
	NoFF          bool   `json:"noFF"`
}

type PromoteRequest struct {
	TargetBranch string `json:"targetBranch"`
	NoFF         bool   `json:"noFF"`
}

type Manager struct {
	repoRoot     string
	worktreeRoot string
	registry     *worktree.Registry
	notify       func(agentID, relPath, changeType, actor string)
}

// Manager 封装 git worktree、merge、promote 等编排动作。
func NewManager(repoRoot, worktreeRoot string, registry *worktree.Registry, notify func(agentID, relPath, changeType, actor string)) *Manager {
	return &Manager{
		repoRoot:     repoRoot,
		worktreeRoot: worktreeRoot,
		registry:     registry,
		notify:       notify,
	}
}

func (m *Manager) ListAgents() []domain.WorktreeInfo {
	states := m.registry.List()
	items := make([]domain.WorktreeInfo, 0, len(states))
	for _, state := range states {
		items = append(items, state.Info())
	}
	slices.SortFunc(items, func(a, b domain.WorktreeInfo) int {
		return strings.Compare(a.AgentID, b.AgentID)
	})
	return items
}

// Prepare 是注册 agent worktree 的唯一入口。
func (m *Manager) Prepare(agentID string, req PrepareRequest) (domain.WorktreeInfo, error) {
	if err := validateAgentID(agentID); err != nil {
		return domain.WorktreeInfo{}, err
	}
	if req.FromRef == "" {
		req.FromRef = "main"
	}
	if req.BranchName == "" {
		req.BranchName = defaultBranchName(agentID)
	}

	rootPath := m.worktreePath(agentID)
	if req.Reset {
		if err := m.removeWorktree(rootPath); err != nil {
			return domain.WorktreeInfo{}, err
		}
	}

	if existing, ok := m.registry.Get(agentID); ok && !req.Reset {
		headSHA, err := m.revParse(existing.RootPath, "HEAD")
		if err != nil {
			return domain.WorktreeInfo{}, err
		}
		existing.HeadSHA = headSHA
		m.registry.Upsert(existing)
		return existing.Info(), nil
	}

	if err := os.MkdirAll(filepath.Dir(rootPath), 0o755); err != nil {
		return domain.WorktreeInfo{}, err
	}
	if _, err := os.Stat(rootPath); err == nil {
		if err := m.removeWorktree(rootPath); err != nil {
			return domain.WorktreeInfo{}, err
		}
	}

	if _, err := m.git(m.repoRoot, nil, "worktree", "add", "-B", req.BranchName, rootPath, req.FromRef); err != nil {
		return domain.WorktreeInfo{}, err
	}
	headSHA, err := m.revParse(rootPath, "HEAD")
	if err != nil {
		return domain.WorktreeInfo{}, err
	}

	state := &worktree.State{
		AgentID:       agentID,
		BranchName:    req.BranchName,
		RootPath:      rootPath,
		HeadSHA:       headSHA,
		PreparedAt:    time.Now().UTC(),
		ActiveExecIDs: make(map[string]struct{}),
	}
	m.registry.Upsert(state)
	return state.Info(), nil
}

func (m *Manager) Status(agentID string) (domain.StatusSummary, error) {
	state, err := m.registry.MustGet(agentID)
	if err != nil {
		return domain.StatusSummary{}, err
	}

	stdout, err := m.git(state.RootPath, nil, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return domain.StatusSummary{}, err
	}
	status := parseStatus(stdout)
	status.BranchName = state.BranchName
	status.HeadSHA, err = m.revParse(state.RootPath, "HEAD")
	if err != nil {
		return domain.StatusSummary{}, err
	}
	m.registry.UpdateHead(agentID, status.HeadSHA)
	return status, nil
}

// Diff 返回给 leader 用的文件摘要和完整 patch。
func (m *Manager) Diff(agentID, baseRef string) (domain.DiffSummary, error) {
	state, err := m.registry.MustGet(agentID)
	if err != nil {
		return domain.DiffSummary{}, err
	}
	if baseRef == "" {
		baseRef = "main"
	}
	rangeRef := fmt.Sprintf("%s...HEAD", baseRef)

	nameStatusOut, err := m.git(state.RootPath, nil, "diff", "--name-status", "--find-renames", rangeRef)
	if err != nil {
		return domain.DiffSummary{}, err
	}
	numStatOut, err := m.git(state.RootPath, nil, "diff", "--numstat", "--find-renames", rangeRef)
	if err != nil {
		return domain.DiffSummary{}, err
	}
	patchOut, err := m.git(state.RootPath, nil, "diff", "--patch", "--find-renames", rangeRef)
	if err != nil {
		return domain.DiffSummary{}, err
	}

	files := combineDiff(parseNameStatus(nameStatusOut), parseNumStat(numStatOut))
	headSHA, err := m.revParse(state.RootPath, "HEAD")
	if err != nil {
		return domain.DiffSummary{}, err
	}
	return domain.DiffSummary{
		BaseRef: baseRef,
		HeadSHA: headSHA,
		Files:   files,
		Patch:   patchOut,
	}, nil
}

// Commit 会先 add 全量改动，再创建一次本地提交。
func (m *Manager) Commit(agentID string, req CommitRequest) (branchName string, commitSHA string, err error) {
	state, err := m.registry.MustGet(agentID)
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(req.Message) == "" {
		return "", "", errors.New("message is required")
	}

	if _, err := m.git(state.RootPath, nil, "add", "-A", "--", "."); err != nil {
		return "", "", err
	}
	staged, err := m.git(state.RootPath, nil, "diff", "--cached", "--name-only")
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(staged) == "" {
		return "", "", domain.ErrGitNoChanges
	}

	env := map[string]string{
		"GIT_AUTHOR_NAME":     defaultString(req.AuthorName, "AgentHub Sandbox"),
		"GIT_AUTHOR_EMAIL":    defaultString(req.AuthorEmail, "sandbox@agenthub.local"),
		"GIT_COMMITTER_NAME":  defaultString(req.AuthorName, "AgentHub Sandbox"),
		"GIT_COMMITTER_EMAIL": defaultString(req.AuthorEmail, "sandbox@agenthub.local"),
	}
	if _, err := m.git(state.RootPath, env, "commit", "-m", req.Message); err != nil {
		return "", "", err
	}

	commitSHA, err = m.revParse(state.RootPath, "HEAD")
	if err != nil {
		return "", "", err
	}
	m.registry.UpdateHead(agentID, commitSHA)
	return state.BranchName, commitSHA, nil
}

// Merge 把 source agent 分支并到 target agent 的 worktree 里。
func (m *Manager) Merge(targetAgentID string, req MergeRequest) (domain.MergeResult, error) {
	target, err := m.registry.MustGet(targetAgentID)
	if err != nil {
		return domain.MergeResult{}, err
	}
	source, err := m.registry.MustGet(req.SourceAgentID)
	if err != nil {
		return domain.MergeResult{}, err
	}

	args := []string{"merge"}
	if req.NoFF {
		args = append(args, "--no-ff")
	}
	args = append(args, source.BranchName, "-m", fmt.Sprintf("merge %s into %s", source.BranchName, target.BranchName))

	_, mergeErr := m.git(target.RootPath, nil, args...)
	if mergeErr != nil {
		status, statusErr := m.Status(targetAgentID)
		if statusErr == nil && len(status.Conflicted) > 0 {
			// 冲突保留在目标 worktree，方便 leader 直接通过 /filesystem 修改。
			for _, conflicted := range status.Conflicted {
				m.notify(targetAgentID, conflicted, "write", "git")
			}
			return domain.MergeResult{
				Status:       "conflicted",
				SourceBranch: source.BranchName,
				TargetBranch: target.BranchName,
				Conflicted:   status.Conflicted,
			}, nil
		}
		return domain.MergeResult{}, mergeErr
	}

	headSHA, err := m.revParse(target.RootPath, "HEAD")
	if err != nil {
		return domain.MergeResult{}, err
	}
	m.registry.UpdateHead(targetAgentID, headSHA)
	changed, err := m.git(target.RootPath, nil, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD")
	if err == nil {
		for _, relPath := range splitLines(changed) {
			m.notify(targetAgentID, relPath, "write", "git")
		}
	}

	return domain.MergeResult{
		Status:         "merged",
		SourceBranch:   source.BranchName,
		TargetBranch:   target.BranchName,
		MergeCommitSHA: headSHA,
	}, nil
}

func (m *Manager) AbortMerge(agentID string) error {
	state, err := m.registry.MustGet(agentID)
	if err != nil {
		return err
	}
	before, _ := m.Status(agentID)
	if _, err := m.git(state.RootPath, nil, "merge", "--abort"); err != nil {
		return err
	}
	for _, relPath := range before.Conflicted {
		m.notify(agentID, relPath, "write", "git")
	}
	return nil
}

// Promote 把某个 agent 分支最终并到主仓库的目标分支里。
func (m *Manager) Promote(agentID string, req PromoteRequest) (domain.PromoteResult, error) {
	state, err := m.registry.MustGet(agentID)
	if err != nil {
		return domain.PromoteResult{}, err
	}
	targetBranch := req.TargetBranch
	if targetBranch == "" {
		targetBranch = "main"
	}

	repoStatus, err := m.git(m.repoRoot, nil, "status", "--porcelain=v1", "--untracked-files=no")
	if err != nil {
		return domain.PromoteResult{}, err
	}
	if strings.TrimSpace(repoStatus) != "" {
		return domain.PromoteResult{}, errors.New("repo root is dirty; refusing to promote")
	}

	if _, err := m.git(m.repoRoot, nil, "checkout", targetBranch); err != nil {
		return domain.PromoteResult{}, err
	}

	args := []string{"merge"}
	if req.NoFF {
		args = append(args, "--no-ff")
	}
	args = append(args, state.BranchName, "-m", fmt.Sprintf("promote %s into %s", state.BranchName, targetBranch))

	if _, err := m.git(m.repoRoot, nil, args...); err != nil {
		// promote 发生冲突时直接回滚 repo root，避免 main 留在半合并状态。
		statusOut, statusErr := m.git(m.repoRoot, nil, "status", "--porcelain=v1", "--untracked-files=no")
		if statusErr == nil {
			status := parseStatus(statusOut)
			if len(status.Conflicted) > 0 {
				_, _ = m.git(m.repoRoot, nil, "merge", "--abort")
				return domain.PromoteResult{
					Status:       "conflicted",
					TargetBranch: targetBranch,
					Conflicted:   status.Conflicted,
				}, nil
			}
		}
		return domain.PromoteResult{}, err
	}

	headSHA, err := m.revParse(m.repoRoot, "HEAD")
	if err != nil {
		return domain.PromoteResult{}, err
	}
	return domain.PromoteResult{
		Status:         "merged",
		TargetBranch:   targetBranch,
		MergeCommitSHA: headSHA,
	}, nil
}

func (m *Manager) DeleteWorktree(agentID string) error {
	if err := validateAgentID(agentID); err != nil {
		return err
	}
	if _, ok := m.registry.Delete(agentID); !ok {
		return m.removeWorktree(m.worktreePath(agentID))
	}
	return m.removeWorktree(m.worktreePath(agentID))
}

func (m *Manager) removeWorktree(rootPath string) error {
	if rootPath == "" {
		return nil
	}
	if err := security.EnsureWithin(m.worktreeRoot, rootPath); err != nil {
		return domain.ErrWorktreeRootNotAllowed
	}

	_, err := m.git(m.repoRoot, nil, "worktree", "remove", "--force", rootPath)
	if err != nil {
		if removeErr := os.RemoveAll(rootPath); removeErr != nil {
			return errors.Join(err, removeErr)
		}
	}
	_, _ = m.git(m.repoRoot, nil, "worktree", "prune")
	return nil
}

func (m *Manager) worktreePath(agentID string) string {
	return filepath.Join(m.worktreeRoot, agentID)
}

func (m *Manager) revParse(cwd string, ref string) (string, error) {
	stdout, err := m.git(cwd, nil, "rev-parse", ref)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout), nil
}

// git 是所有 git 子命令的统一入口，方便控制 cwd、env 和错误格式。
func (m *Manager) git(cwd string, env map[string]string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	cmd.Env = os.Environ()
	for key, value := range env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", key, value))
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		errText := strings.TrimSpace(stderr.String())
		if errText == "" {
			errText = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), errText)
	}
	return stdout.String(), nil
}

func validateAgentID(agentID string) error {
	if !agentIDPattern.MatchString(agentID) {
		return domain.ErrAgentIDRequired
	}
	return nil
}

func defaultBranchName(agentID string) string {
	return fmt.Sprintf("agent/%s", agentID)
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// parseStatus 会把 git porcelain 输出转成前端更容易消费的结构。
func parseStatus(stdout string) domain.StatusSummary {
	status := domain.StatusSummary{
		Staged:     []string{},
		Unstaged:   []string{},
		Untracked:  []string{},
		Conflicted: []string{},
	}
	for _, line := range splitLines(stdout) {
		if strings.HasPrefix(line, "?? ") {
			status.Untracked = append(status.Untracked, strings.TrimSpace(line[3:]))
			continue
		}
		if len(line) < 3 {
			continue
		}
		x := line[0]
		y := line[1]
		path := normalizeStatusPath(strings.TrimSpace(line[3:]))
		if isConflictStatus(x, y) {
			status.Conflicted = append(status.Conflicted, path)
			continue
		}
		if x != ' ' && x != '?' {
			status.Staged = append(status.Staged, path)
		}
		if y != ' ' && y != '?' {
			status.Unstaged = append(status.Unstaged, path)
		}
	}
	status.Staged = uniqueSorted(status.Staged)
	status.Unstaged = uniqueSorted(status.Unstaged)
	status.Untracked = uniqueSorted(status.Untracked)
	status.Conflicted = uniqueSorted(status.Conflicted)
	return status
}

func parseNameStatus(stdout string) map[string]string {
	items := make(map[string]string)
	for _, line := range splitLines(stdout) {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		status := fields[0]
		path := fields[len(fields)-1]
		items[normalizeStatusPath(path)] = status
	}
	return items
}

func parseNumStat(stdout string) map[string][2]int {
	items := make(map[string][2]int)
	for _, line := range splitLines(stdout) {
		parts := strings.Split(line, "\t")
		if len(parts) < 3 {
			continue
		}
		additions, _ := strconv.Atoi(parts[0])
		deletions, _ := strconv.Atoi(parts[1])
		items[normalizeStatusPath(parts[2])] = [2]int{additions, deletions}
	}
	return items
}

func combineDiff(nameStatus map[string]string, numStat map[string][2]int) []domain.DiffFile {
	seen := make(map[string]struct{})
	files := make([]domain.DiffFile, 0, len(nameStatus)+len(numStat))
	for path, status := range nameStatus {
		stats := numStat[path]
		files = append(files, domain.DiffFile{
			Path:      path,
			Status:    status,
			Additions: stats[0],
			Deletions: stats[1],
		})
		seen[path] = struct{}{}
	}
	for path, stats := range numStat {
		if _, ok := seen[path]; ok {
			continue
		}
		files = append(files, domain.DiffFile{
			Path:      path,
			Status:    "M",
			Additions: stats[0],
			Deletions: stats[1],
		})
	}
	slices.SortFunc(files, func(a, b domain.DiffFile) int {
		return strings.Compare(a.Path, b.Path)
	})
	return files
}

func isConflictStatus(x, y byte) bool {
	if x == 'U' || y == 'U' {
		return true
	}
	return (x == 'A' && y == 'A') || (x == 'D' && y == 'D')
}

func normalizeStatusPath(path string) string {
	if strings.Contains(path, " -> ") {
		parts := strings.Split(path, " -> ")
		path = parts[len(parts)-1]
	}
	return filepath.ToSlash(filepath.Clean(path))
}

func splitLines(stdout string) []string {
	lines := strings.Split(strings.ReplaceAll(stdout, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}

func uniqueSorted(items []string) []string {
	if len(items) == 0 {
		return items
	}
	slices.Sort(items)
	result := items[:0]
	var last string
	for i, item := range items {
		if i == 0 || item != last {
			result = append(result, item)
			last = item
		}
	}
	return result
}
