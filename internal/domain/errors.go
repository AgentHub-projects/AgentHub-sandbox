package domain

import "errors"

var (
	ErrAgentIDRequired        = errors.New("agent id is required")
	ErrWorktreeNotPrepared    = errors.New("agent worktree is not prepared")
	ErrInvalidPath            = errors.New("invalid path")
	ErrInvalidCommit          = errors.New("invalid commit")
	ErrInvalidEdit            = errors.New("invalid edit range")
	ErrPathEscapesWorktree    = errors.New("path escapes workspace")
	ErrVersionConflict        = errors.New("version conflict")
	ErrExecNotFound           = errors.New("exec not found")
	ErrGitNoChanges           = errors.New("no changes to commit")
	ErrMergeConflict          = errors.New("merge conflict")
	ErrWorktreeRootNotAllowed = errors.New("worktree root not allowed")
)
