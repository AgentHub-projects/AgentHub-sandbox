package domain

import "time"

const MainWorkspaceID = "main"

type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type ResponseEnvelope struct {
	RequestID string    `json:"requestId,omitempty"`
	OK        bool      `json:"ok"`
	Data      any       `json:"data,omitempty"`
	Error     *APIError `json:"error,omitempty"`
}

type FileEntry struct {
	Path    string        `json:"path"`
	Name    string        `json:"name"`
	Kind    string        `json:"kind"`
	Size    int64         `json:"size"`
	Mtime   time.Time     `json:"mtime"`
	Version string        `json:"version,omitempty"`
	Preview *ImagePreview `json:"preview,omitempty"`
}

type ImagePreview struct {
	Path         string    `json:"path,omitempty"`
	PreviewURL   string    `json:"previewUrl"`
	OssURI       string    `json:"ossUri,omitempty"`
	OssObjectKey string    `json:"ossObjectKey,omitempty"`
	MimeType     string    `json:"mimeType,omitempty"`
	SizeBytes    int64     `json:"sizeBytes,omitempty"`
	Width        int       `json:"width,omitempty"`
	Height       int       `json:"height,omitempty"`
	SHA256       string    `json:"sha256,omitempty"`
	SessionID    string    `json:"sessionId,omitempty"`
	UpdatedAt    time.Time `json:"updatedAt,omitempty"`
}

type WorktreeInfo struct {
	AgentID    string    `json:"agentId"`
	BranchName string    `json:"branchName"`
	RootPath   string    `json:"rootPath"`
	HeadSHA    string    `json:"headSha"`
	PreparedAt time.Time `json:"preparedAt"`
}

type StatusSummary struct {
	BranchName string   `json:"branchName"`
	HeadSHA    string   `json:"headSha"`
	Staged     []string `json:"staged"`
	Unstaged   []string `json:"unstaged"`
	Untracked  []string `json:"untracked"`
	Conflicted []string `json:"conflicted"`
}

type AgentCompletionResult struct {
	Status     string   `json:"status"`
	BranchName string   `json:"branchName"`
	HeadSHA    string   `json:"headSha,omitempty"`
	CommitSHA  string   `json:"commitSha,omitempty"`
	Patch      string   `json:"patch,omitempty"`
	Staged     []string `json:"staged,omitempty"`
	Unstaged   []string `json:"unstaged,omitempty"`
	Untracked  []string `json:"untracked,omitempty"`
	Conflicted []string `json:"conflicted,omitempty"`
}

type DiffFile struct {
	Path      string `json:"path"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

type DiffSummary struct {
	BaseRef string     `json:"baseRef"`
	HeadSHA string     `json:"headSha"`
	Files   []DiffFile `json:"files"`
	Patch   string     `json:"patch"`
}

type MainCommitEvent struct {
	BranchName      string    `json:"branchName"`
	CommitSHA       string    `json:"commitSha"`
	ParentCommitSHA string    `json:"parentCommitSha,omitempty"`
	CommittedAt     time.Time `json:"committedAt"`
	Comment         string    `json:"comment"`
}

type MainCommitItem struct {
	CommitSHA   string    `json:"commitSha"`
	CommittedAt time.Time `json:"committedAt"`
	Comment     string    `json:"comment"`
}

type MainCommitList struct {
	BranchName string           `json:"branchName"`
	Items      []MainCommitItem `json:"items"`
	HasMore    bool             `json:"hasMore"`
	NextCursor string           `json:"nextCursor,omitempty"`
}

type MainDiffFile struct {
	Path      string `json:"path"`
	OldPath   string `json:"oldPath,omitempty"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

type MainCommitDiffFiles struct {
	BranchName      string         `json:"branchName"`
	CommitSHA       string         `json:"commitSha"`
	ParentCommitSHA string         `json:"parentCommitSha,omitempty"`
	Files           []MainDiffFile `json:"files"`
}

type MainDiffBaseFile struct {
	Path     string  `json:"path"`
	Exists   bool    `json:"exists"`
	Content  *string `json:"content"`
	IsBinary bool    `json:"isBinary"`
}

type MainCommitDiffFile struct {
	BranchName      string           `json:"branchName"`
	CommitSHA       string           `json:"commitSha"`
	ParentCommitSHA string           `json:"parentCommitSha,omitempty"`
	Path            string           `json:"path"`
	OldPath         string           `json:"oldPath,omitempty"`
	Status          string           `json:"status"`
	BaseFile        MainDiffBaseFile `json:"baseFile"`
	Patch           string           `json:"patch"`
}

type MergeResult struct {
	Status         string   `json:"status"`
	SourceBranch   string   `json:"sourceBranch,omitempty"`
	TargetBranch   string   `json:"targetBranch"`
	MergeCommitSHA string   `json:"mergeCommitSha,omitempty"`
	Conflicted     []string `json:"conflicted,omitempty"`
}

type SyncResult struct {
	Status         string   `json:"status"`
	SourceRef      string   `json:"sourceRef"`
	TargetBranch   string   `json:"targetBranch"`
	HeadSHA        string   `json:"headSha,omitempty"`
	MergeCommitSHA string   `json:"mergeCommitSha,omitempty"`
	Dirty          []string `json:"dirty,omitempty"`
	Conflicted     []string `json:"conflicted,omitempty"`
}

type PromoteResult struct {
	Status         string   `json:"status"`
	TargetBranch   string   `json:"targetBranch"`
	MergeCommitSHA string   `json:"mergeCommitSha,omitempty"`
	Conflicted     []string `json:"conflicted,omitempty"`
}
