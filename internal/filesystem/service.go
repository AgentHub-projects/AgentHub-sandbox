package filesystem

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"agenthub-sandbox/internal/domain"
	"agenthub-sandbox/internal/security"
	"agenthub-sandbox/internal/watcher"
	"agenthub-sandbox/internal/worktree"
)

type ReadResult struct {
	Path    string    `json:"path"`
	Content string    `json:"content"`
	Size    int64     `json:"size"`
	Mtime   time.Time `json:"mtime"`
	Version string    `json:"version"`
}

type WriteResult struct {
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	Mtime   time.Time `json:"mtime"`
	Version string    `json:"version"`
}

type Service struct {
	registry *worktree.Registry
	hub      *watcher.Hub
	watcher  *fsnotify.Watcher

	mu             sync.Mutex
	agentRoots     map[string]string
	watchedByAgent map[string]map[string]struct{}
	recent         map[string]time.Time
}

// Service 负责 worktree 内的安全读写、目录监听和变更广播。
func NewService(registry *worktree.Registry, hub *watcher.Hub) (*Service, error) {
	fsWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create fsnotify watcher: %w", err)
	}

	service := &Service{
		registry:       registry,
		hub:            hub,
		watcher:        fsWatcher,
		agentRoots:     make(map[string]string),
		watchedByAgent: make(map[string]map[string]struct{}),
		recent:         make(map[string]time.Time),
	}
	go service.loop()
	return service, nil
}

func (s *Service) Close() error {
	return s.watcher.Close()
}

func (s *Service) Hub() *watcher.Hub {
	return s.hub
}

// SyncAgent 会把某个 agent 的整个 worktree 目录树挂到 fsnotify 上。
func (s *Service) SyncAgent(agentID string) error {
	state, err := s.registry.MustGet(agentID)
	if err != nil {
		return err
	}

	s.mu.Lock()
	if err := s.removeAgentLocked(agentID); err != nil {
		s.mu.Unlock()
		return err
	}
	s.agentRoots[agentID] = state.RootPath
	s.watchedByAgent[agentID] = make(map[string]struct{})
	s.mu.Unlock()

	watched := make([]string, 0, 16)
	err = filepath.WalkDir(state.RootPath, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() {
			return nil
		}
		if err := s.watcher.Add(path); err != nil {
			return err
		}
		watched = append(watched, path)
		return nil
	})
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.watchedByAgent[agentID] == nil {
		s.watchedByAgent[agentID] = make(map[string]struct{}, len(watched))
	}
	for _, path := range watched {
		s.watchedByAgent[agentID][path] = struct{}{}
	}
	return nil
}

func (s *Service) RemoveAgent(agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.removeAgentLocked(agentID)
}

func (s *Service) Info(agentID string) (domain.WorktreeInfo, error) {
	state, err := s.registry.MustGet(agentID)
	if err != nil {
		return domain.WorktreeInfo{}, err
	}
	return state.Info(), nil
}

// List 用来给前端代码树展示目录和文件信息。
func (s *Service) List(agentID, rawPath string, depth int) ([]domain.FileEntry, error) {
	state, err := s.registry.MustGet(agentID)
	if err != nil {
		return nil, err
	}
	resolved, rel, err := security.ResolveExisting(state.RootPath, rawPath)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []domain.FileEntry{fileEntry(info, rel)}, nil
	}
	if depth <= 0 {
		depth = 1
	}

	entries := make([]domain.FileEntry, 0)
	rootDepth := strings.Count(filepath.ToSlash(resolved), "/")
	err = filepath.WalkDir(resolved, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == resolved {
			return nil
		}

		currentDepth := strings.Count(filepath.ToSlash(path), "/") - rootDepth
		if currentDepth > depth {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		itemRel, err := filepath.Rel(state.RootPath, path)
		if err != nil {
			return err
		}
		entries = append(entries, fileEntry(info, filepath.ToSlash(itemRel)))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Path < entries[j].Path
	})
	return entries, nil
}

// Read 支持整文件读取，也支持按行裁剪返回。
func (s *Service) Read(agentID, rawPath string, lineStart, lineEnd int) (ReadResult, error) {
	state, err := s.registry.MustGet(agentID)
	if err != nil {
		return ReadResult{}, err
	}
	resolved, rel, err := security.ResolveExisting(state.RootPath, rawPath)
	if err != nil {
		return ReadResult{}, err
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return ReadResult{}, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return ReadResult{}, err
	}

	content := sliceLines(string(data), lineStart, lineEnd)
	return ReadResult{
		Path:    rel,
		Content: content,
		Size:    info.Size(),
		Mtime:   info.ModTime().UTC(),
		Version: hashBytes(data),
	}, nil
}

// Write 会校验版本、限制路径，并在写盘后主动广播变更事件。
func (s *Service) Write(agentID, rawPath, content, expectedVersion string, createDirs bool, actor string) (WriteResult, error) {
	state, err := s.registry.MustGet(agentID)
	if err != nil {
		return WriteResult{}, err
	}
	resolved, rel, err := security.ResolveForWrite(state.RootPath, rawPath)
	if err != nil {
		return WriteResult{}, err
	}

	if expectedVersion != "" {
		current, err := os.ReadFile(resolved)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return WriteResult{}, err
		}
		if err == nil && hashBytes(current) != expectedVersion {
			return WriteResult{}, domain.ErrVersionConflict
		}
	}

	parentDir := filepath.Dir(resolved)
	if createDirs {
		if err := os.MkdirAll(parentDir, 0o755); err != nil {
			return WriteResult{}, err
		}
	} else if _, err := os.Stat(parentDir); err != nil {
		return WriteResult{}, err
	}

	if err := os.WriteFile(resolved, []byte(content), 0o644); err != nil {
		return WriteResult{}, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return WriteResult{}, err
	}

	version := hashBytes([]byte(content))
	s.markRecent(resolved)
	s.hub.Broadcast(watcher.Event{
		AgentID:    agentID,
		Path:       rel,
		ChangeType: "write",
		Mtime:      info.ModTime().UTC(),
		Version:    version,
		Actor:      actor,
	})

	return WriteResult{
		Path:    rel,
		Size:    info.Size(),
		Mtime:   info.ModTime().UTC(),
		Version: version,
	}, nil
}

// NotifyChange 给 git 等非直接文件写入路径复用统一的变更通知逻辑。
func (s *Service) NotifyChange(agentID, relPath, changeType, actor string) {
	state, err := s.registry.MustGet(agentID)
	if err != nil {
		return
	}
	resolved := filepath.Join(state.RootPath, filepath.FromSlash(relPath))
	s.markRecent(resolved)
	version, mtime := s.fileMeta(resolved)
	s.hub.Broadcast(watcher.Event{
		AgentID:    agentID,
		Path:       filepath.ToSlash(filepath.Clean(filepath.FromSlash(relPath))),
		ChangeType: changeType,
		Mtime:      mtime,
		Version:    version,
		Actor:      actor,
	})
}

// loop 持续消费 fsnotify 事件并转发成 sandbox 内部的文件变更消息。
func (s *Service) loop() {
	for {
		select {
		case event, ok := <-s.watcher.Events:
			if !ok {
				return
			}
			s.handleEvent(event)
		case _, ok := <-s.watcher.Errors:
			if !ok {
				return
			}
		}
	}
}

func (s *Service) handleEvent(event fsnotify.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 跳过本服务自己触发的回环事件，避免前端收到重复通知。
	if s.consumeRecent(event.Name) {
		return
	}
	agentID, rootPath, ok := s.agentForPathLocked(event.Name)
	if !ok {
		return
	}

	if event.Has(fsnotify.Create) {
		if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
			_ = filepath.WalkDir(event.Name, func(path string, d fs.DirEntry, walkErr error) error {
				if walkErr != nil || !d.IsDir() {
					return walkErr
				}
				if err := s.watcher.Add(path); err == nil {
					s.watchedByAgent[agentID][path] = struct{}{}
				}
				return nil
			})
		}
	}
	if event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
		if watched := s.watchedByAgent[agentID]; watched != nil {
			delete(watched, event.Name)
		}
	}

	rel, err := filepath.Rel(rootPath, event.Name)
	if err != nil {
		return
	}
	version, mtime := s.fileMeta(event.Name)
	s.hub.Broadcast(watcher.Event{
		AgentID:    agentID,
		Path:       filepath.ToSlash(rel),
		ChangeType: changeType(event),
		Mtime:      mtime,
		Version:    version,
		Actor:      "agent",
	})
}

func (s *Service) agentForPathLocked(path string) (string, string, bool) {
	for agentID, rootPath := range s.agentRoots {
		if err := security.EnsureWithin(rootPath, path); err == nil {
			return agentID, rootPath, true
		}
	}
	return "", "", false
}

func (s *Service) removeAgentLocked(agentID string) error {
	for watched := range s.watchedByAgent[agentID] {
		if err := s.watcher.Remove(watched); err != nil && !errors.Is(err, fsnotify.ErrNonExistentWatch) {
			return err
		}
	}
	delete(s.watchedByAgent, agentID)
	delete(s.agentRoots, agentID)
	return nil
}

// recent 用一个短时间窗口去重本地写入产生的回调。
func (s *Service) markRecent(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recent[filepath.Clean(path)] = time.Now().Add(750 * time.Millisecond)
}

func (s *Service) consumeRecent(path string) bool {
	now := time.Now()
	cleaned := filepath.Clean(path)
	expiresAt, ok := s.recent[cleaned]
	if !ok {
		return false
	}
	if now.After(expiresAt) {
		delete(s.recent, cleaned)
		return false
	}
	delete(s.recent, cleaned)
	return true
}

func (s *Service) fileMeta(path string) (string, time.Time) {
	info, err := os.Stat(path)
	if err != nil {
		return "", time.Time{}
	}
	if info.IsDir() {
		return "", info.ModTime().UTC()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", info.ModTime().UTC()
	}
	return hashBytes(data), info.ModTime().UTC()
}

func fileEntry(info fs.FileInfo, rel string) domain.FileEntry {
	kind := "file"
	if info.IsDir() {
		kind = "dir"
	}
	return domain.FileEntry{
		Path:    filepath.ToSlash(rel),
		Name:    info.Name(),
		Kind:    kind,
		Size:    info.Size(),
		Mtime:   info.ModTime().UTC(),
		Version: statVersion(info),
	}
}

func statVersion(info fs.FileInfo) string {
	return fmt.Sprintf("%d-%d", info.ModTime().UnixNano(), info.Size())
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func changeType(event fsnotify.Event) string {
	switch {
	case event.Has(fsnotify.Remove):
		return "remove"
	case event.Has(fsnotify.Rename):
		return "rename"
	case event.Has(fsnotify.Create):
		return "create"
	default:
		return "write"
	}
}

func sliceLines(content string, lineStart, lineEnd int) string {
	if lineStart <= 0 && lineEnd <= 0 {
		return content
	}
	lines := strings.SplitAfter(content, "\n")
	start := 0
	if lineStart > 1 {
		start = lineStart - 1
		if start > len(lines) {
			start = len(lines)
		}
	}
	end := len(lines)
	if lineEnd > 0 && lineEnd < end {
		end = lineEnd
	}
	if start > end {
		start = end
	}
	return strings.Join(lines[start:end], "")
}
