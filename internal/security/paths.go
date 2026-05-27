package security

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agenthub-sandbox/internal/domain"
)

func NormalizeRelativePath(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return ".", nil
	}

	cleaned := filepath.Clean(filepath.FromSlash(raw))
	if filepath.IsAbs(cleaned) || filepath.VolumeName(cleaned) != "" {
		return "", domain.ErrInvalidPath
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", domain.ErrPathEscapesWorktree
	}
	return filepath.ToSlash(cleaned), nil
}

// ResolveExisting 用在读文件场景，要求目标路径本身已经存在。
func ResolveExisting(rootPath, raw string) (resolved string, rel string, err error) {
	rootReal, err := filepath.EvalSymlinks(rootPath)
	if err != nil {
		return "", "", fmt.Errorf("resolve worktree root: %w", err)
	}
	rel, err = NormalizeRelativePath(raw)
	if err != nil {
		return "", "", err
	}

	target := filepath.Join(rootReal, filepath.FromSlash(rel))
	targetReal, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", "", err
	}
	if err := EnsureWithin(rootReal, targetReal); err != nil {
		return "", "", err
	}
	return targetReal, rel, nil
}

// ResolveForWrite 用在写文件场景，重点校验父目录和最终路径不能逃出 worktree。
func ResolveForWrite(rootPath, raw string) (resolved string, rel string, err error) {
	rootReal, err := filepath.EvalSymlinks(rootPath)
	if err != nil {
		return "", "", fmt.Errorf("resolve worktree root: %w", err)
	}
	rel, err = NormalizeRelativePath(raw)
	if err != nil {
		return "", "", err
	}

	target := filepath.Join(rootReal, filepath.FromSlash(rel))
	parent := filepath.Dir(target)
	parentReal, err := filepath.EvalSymlinks(parent)
	if err != nil {
		if os.IsNotExist(err) {
			parentReal = parent
		} else {
			return "", "", err
		}
	}
	if err := EnsureWithin(rootReal, parentReal); err != nil {
		return "", "", err
	}
	resolved = filepath.Join(parentReal, filepath.Base(target))
	if err := EnsureWithin(rootReal, resolved); err != nil {
		return "", "", err
	}
	return resolved, rel, nil
}

// EnsureWithin 是所有路径安全检查的最后一道兜底。
func EnsureWithin(rootPath, targetPath string) error {
	rootClean := filepath.Clean(rootPath)
	targetClean := filepath.Clean(targetPath)
	rel, err := filepath.Rel(rootClean, targetClean)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return domain.ErrPathEscapesWorktree
	}
	return nil
}
