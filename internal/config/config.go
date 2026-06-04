package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

type Config struct {
	Host         string
	Port         int
	RepoRoot     string
	WorktreeRoot string
}

func Load() (Config, error) {
	cfg := Config{
		Host:         stringOrDefault(os.Getenv("HOST"), "0.0.0.0"),
		Port:         intOrDefault(os.Getenv("PORT"), 8080),
		RepoRoot:     stringOrDefault(os.Getenv("REPO_ROOT"), "/sandbox/views/workspace/repo"),
		WorktreeRoot: stringOrDefault(os.Getenv("WORKTREE_ROOT"), "/sandbox/views/workspace/worktrees"),
	}

	repoRoot, err := filepath.Abs(cfg.RepoRoot)
	if err != nil {
		return Config{}, fmt.Errorf("resolve REPO_ROOT: %w", err)
	}
	worktreeRoot, err := filepath.Abs(cfg.WorktreeRoot)
	if err != nil {
		return Config{}, fmt.Errorf("resolve WORKTREE_ROOT: %w", err)
	}

	cfg.RepoRoot = repoRoot
	cfg.WorktreeRoot = worktreeRoot
	return cfg, nil
}

func (c Config) Address() string {
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}

func stringOrDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func intOrDefault(raw string, fallback int) int {
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}
