package filesystem

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

const (
	agentHubMetadataDir = ".agenthub"
	imageManifestPath   = ".agenthub/image-manifest.json"
)

type ReadResult struct {
	Path    string               `json:"path"`
	Content string               `json:"content"`
	Size    int64                `json:"size"`
	Mtime   time.Time            `json:"mtime"`
	Version string               `json:"version"`
	Preview *domain.ImagePreview `json:"preview,omitempty"`
}

type ReadBytesResult struct {
	Path    string
	Content []byte
	Size    int64
	Mtime   time.Time
	Version string
	Preview *domain.ImagePreview
}

type WriteResult struct {
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	Mtime   time.Time `json:"mtime"`
	Version string    `json:"version"`
}

type TextEdit struct {
	StartLine   int    `json:"startLine"`
	StartColumn int    `json:"startColumn"`
	EndLine     int    `json:"endLine"`
	EndColumn   int    `json:"endColumn"`
	Text        string `json:"text"`
}

type Service struct {
	registry *worktree.Registry
	hub      *watcher.Hub
	watcher  *fsnotify.Watcher

	mu             sync.Mutex
	manifestMu     sync.Mutex
	agentRoots     map[string]string
	watchedByAgent map[string]map[string]struct{}
	recent         map[string]time.Time
	ensureAgent    func(agentID string) error
}

type imageManifest struct {
	Version int                            `json:"version"`
	Images  map[string]domain.ImagePreview `json:"images"`
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

func (s *Service) SetEnsureAgent(ensure func(agentID string) error) {
	s.ensureAgent = ensure
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
		if skipInternalMetadataDir(d) {
			return filepath.SkipDir
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
	state, err := s.state(agentID)
	if err != nil {
		return domain.WorktreeInfo{}, err
	}
	return state.Info(), nil
}

// List 用来给前端代码树展示目录和文件信息。
func (s *Service) List(agentID, rawPath string, depth int) ([]domain.FileEntry, error) {
	state, err := s.state(agentID)
	if err != nil {
		return nil, err
	}
	resolved, rel, err := security.ResolveExisting(state.RootPath, rawPath)
	if err != nil {
		return nil, err
	}
	if isInternalMetadataPath(rel) {
		return nil, domain.ErrInvalidPath
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		manifest := s.readImageManifestBestEffort(state.RootPath)
		return []domain.FileEntry{fileEntry(info, rel, imagePreviewForRel(manifest, rel))}, nil
	}
	if depth <= 0 {
		depth = 1
	}

	entries := make([]domain.FileEntry, 0)
	manifest := s.readImageManifestBestEffort(state.RootPath)
	rootReal, err := filepath.EvalSymlinks(state.RootPath)
	if err != nil {
		return nil, err
	}
	rootDepth := strings.Count(filepath.ToSlash(resolved), "/")
	err = filepath.WalkDir(resolved, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == resolved {
			return nil
		}
		if skipInternalMetadataDir(d) {
			return filepath.SkipDir
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
		itemRel, err := filepath.Rel(rootReal, path)
		if err != nil {
			return err
		}
		relPath := filepath.ToSlash(itemRel)
		entries = append(entries, fileEntry(info, relPath, imagePreviewForRel(manifest, relPath)))
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
	state, err := s.state(agentID)
	if err != nil {
		return ReadResult{}, err
	}
	resolved, rel, err := security.ResolveExisting(state.RootPath, rawPath)
	if err != nil {
		return ReadResult{}, err
	}
	if isInternalMetadataPath(rel) {
		return ReadResult{}, domain.ErrInvalidPath
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return ReadResult{}, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return ReadResult{}, err
	}

	preview := imagePreviewForData(s.readImageManifestBestEffort(state.RootPath), rel, data)
	content := ""
	if preview == nil {
		content = sliceLines(string(data), lineStart, lineEnd)
	}
	return ReadResult{
		Path:    rel,
		Content: content,
		Size:    info.Size(),
		Mtime:   info.ModTime().UTC(),
		Version: hashBytes(data),
		Preview: preview,
	}, nil
}

func (s *Service) ReadBytes(agentID, rawPath string) (ReadBytesResult, error) {
	state, err := s.state(agentID)
	if err != nil {
		return ReadBytesResult{}, err
	}
	resolved, rel, err := security.ResolveExisting(state.RootPath, rawPath)
	if err != nil {
		return ReadBytesResult{}, err
	}
	if isInternalMetadataPath(rel) {
		return ReadBytesResult{}, domain.ErrInvalidPath
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return ReadBytesResult{}, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return ReadBytesResult{}, err
	}
	return ReadBytesResult{
		Path:    rel,
		Content: data,
		Size:    info.Size(),
		Mtime:   info.ModTime().UTC(),
		Version: hashBytes(data),
		Preview: imagePreviewForData(s.readImageManifestBestEffort(state.RootPath), rel, data),
	}, nil
}

func (s *Service) UpsertImagePreview(agentID string, preview domain.ImagePreview) (domain.ImagePreview, error) {
	state, err := s.state(agentID)
	if err != nil {
		return domain.ImagePreview{}, err
	}
	if strings.TrimSpace(preview.Path) == "" || strings.TrimSpace(preview.PreviewURL) == "" {
		return domain.ImagePreview{}, domain.ErrInvalidPath
	}
	resolved, rel, err := security.ResolveExisting(state.RootPath, preview.Path)
	if err != nil {
		return domain.ImagePreview{}, err
	}
	if isInternalMetadataPath(rel) {
		return domain.ImagePreview{}, domain.ErrInvalidPath
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return domain.ImagePreview{}, err
	}
	if info.IsDir() {
		return domain.ImagePreview{}, domain.ErrInvalidPath
	}

	preview.Path = rel
	if preview.SizeBytes == 0 {
		preview.SizeBytes = info.Size()
	}
	if preview.UpdatedAt.IsZero() {
		preview.UpdatedAt = time.Now().UTC()
	}

	s.manifestMu.Lock()
	defer s.manifestMu.Unlock()

	if err := upsertImageManifestEntry(state.RootPath, rel, preview); err != nil {
		return domain.ImagePreview{}, err
	}
	if agentID != domain.MainWorkspaceID {
		mainState, err := s.state(domain.MainWorkspaceID)
		if err != nil {
			return domain.ImagePreview{}, err
		}
		if err := upsertImageManifestEntry(mainState.RootPath, rel, preview); err != nil {
			return domain.ImagePreview{}, err
		}
	}
	return preview, nil
}

func (s *Service) ImagePreview(agentID, rawPath string) (*domain.ImagePreview, error) {
	state, err := s.state(agentID)
	if err != nil {
		return nil, err
	}
	resolved, rel, err := security.ResolveExisting(state.RootPath, rawPath)
	if err != nil {
		return nil, err
	}
	if isInternalMetadataPath(rel) {
		return nil, domain.ErrInvalidPath
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return nil, err
	}
	manifest, err := s.readImageManifest(state.RootPath)
	if err != nil {
		return nil, err
	}
	return imagePreviewForData(manifest, rel, data), nil
}

// Write 会校验版本、限制路径，并在写盘后主动广播变更事件。
func (s *Service) Write(agentID, rawPath, content, expectedVersion string, createDirs bool, actor string) (WriteResult, error) {
	return s.WriteBytes(agentID, rawPath, []byte(content), expectedVersion, createDirs, actor)
}

func (s *Service) WriteBytes(agentID, rawPath string, content []byte, expectedVersion string, createDirs bool, actor string) (WriteResult, error) {
	state, err := s.state(agentID)
	if err != nil {
		return WriteResult{}, err
	}
	resolved, rel, err := security.ResolveForWrite(state.RootPath, rawPath)
	if err != nil {
		return WriteResult{}, err
	}
	if isInternalMetadataPath(rel) {
		return WriteResult{}, domain.ErrInvalidPath
	}

	if _, err := currentContent(resolved, expectedVersion); err != nil {
		return WriteResult{}, err
	}
	if err := ensureParentDir(resolved, createDirs); err != nil {
		return WriteResult{}, err
	}
	return s.writeResolved(agentID, resolved, rel, content, actor)
}

// ApplyEdits 接收前端提交的局部文本变更，避免用户侧保存文件时传输整份内容。
func (s *Service) ApplyEdits(agentID, rawPath, expectedVersion string, edits []TextEdit, createDirs bool, actor string) (WriteResult, error) {
	state, err := s.state(agentID)
	if err != nil {
		return WriteResult{}, err
	}
	resolved, rel, err := security.ResolveForWrite(state.RootPath, rawPath)
	if err != nil {
		return WriteResult{}, err
	}
	if isInternalMetadataPath(rel) {
		return WriteResult{}, domain.ErrInvalidPath
	}

	current, err := currentContent(resolved, expectedVersion)
	if err != nil {
		return WriteResult{}, err
	}
	updated, err := applyTextEdits(string(current), edits)
	if err != nil {
		return WriteResult{}, err
	}
	if err := ensureParentDir(resolved, createDirs); err != nil {
		return WriteResult{}, err
	}
	return s.writeResolved(agentID, resolved, rel, []byte(updated), actor)
}

func currentContent(resolved, expectedVersion string) ([]byte, error) {
	current, err := os.ReadFile(resolved)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		current = []byte{}
	}
	if expectedVersion != "" && hashBytes(current) != expectedVersion {
		return nil, domain.ErrVersionConflict
	}
	return current, nil
}

func ensureParentDir(resolved string, createDirs bool) error {
	parentDir := filepath.Dir(resolved)
	if createDirs {
		return os.MkdirAll(parentDir, 0o755)
	}
	_, err := os.Stat(parentDir)
	return err
}

func (s *Service) writeResolved(agentID, resolved, rel string, content []byte, actor string) (WriteResult, error) {
	if err := os.WriteFile(resolved, content, 0o644); err != nil {
		return WriteResult{}, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return WriteResult{}, err
	}

	version := hashBytes(content)
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
	if isInternalMetadataPath(relPath) {
		return
	}
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

func (s *Service) state(agentID string) (*worktree.State, error) {
	state, err := s.registry.MustGet(agentID)
	if err == nil {
		if s.ensureAgent != nil && !s.isSynced(agentID) {
			if err := s.ensureAgent(agentID); err != nil {
				return nil, err
			}
			return s.registry.MustGet(agentID)
		}
		return state, nil
	}
	if !errors.Is(err, domain.ErrWorktreeNotPrepared) || s.ensureAgent == nil {
		return nil, err
	}
	if err := s.ensureAgent(agentID); err != nil {
		return nil, err
	}
	return s.registry.MustGet(agentID)
}

func (s *Service) isSynced(agentID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agentRoots[agentID] != ""
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
				if skipInternalMetadataDir(d) {
					return filepath.SkipDir
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
	if isInternalMetadataPath(rel) {
		return
	}
	version, mtime := s.fileMeta(event.Name)
	s.hub.Broadcast(watcher.Event{
		AgentID:    agentID,
		Path:       filepath.ToSlash(rel),
		ChangeType: changeType(event),
		Mtime:      mtime,
		Version:    version,
		Actor:      fsnotifyActor(agentID),
	})
}

func fsnotifyActor(agentID string) string {
	if agentID == domain.MainWorkspaceID {
		return "workspace"
	}
	return "agent"
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

func fileEntry(info fs.FileInfo, rel string, preview *domain.ImagePreview) domain.FileEntry {
	kind := "file"
	if info.IsDir() {
		kind = "dir"
		preview = nil
	}
	return domain.FileEntry{
		Path:    filepath.ToSlash(rel),
		Name:    info.Name(),
		Kind:    kind,
		Size:    info.Size(),
		Mtime:   info.ModTime().UTC(),
		Version: statVersion(info),
		Preview: preview,
	}
}

func isInternalMetadataPath(rel string) bool {
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	return cleaned == ".git" ||
		strings.HasPrefix(cleaned, ".git/") ||
		cleaned == agentHubMetadataDir ||
		strings.HasPrefix(cleaned, agentHubMetadataDir+"/")
}

func skipInternalMetadataDir(entry fs.DirEntry) bool {
	return entry.IsDir() && (entry.Name() == ".git" || entry.Name() == agentHubMetadataDir)
}

func (s *Service) readImageManifest(rootPath string) (*imageManifest, error) {
	s.manifestMu.Lock()
	defer s.manifestMu.Unlock()
	return readImageManifestFile(rootPath)
}

func (s *Service) readImageManifestBestEffort(rootPath string) *imageManifest {
	manifest, err := s.readImageManifest(rootPath)
	if err != nil {
		return nil
	}
	return manifest
}

func readImageManifestFile(rootPath string) (*imageManifest, error) {
	resolved, _, err := security.ResolveExisting(rootPath, imageManifestPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &imageManifest{Version: 1, Images: map[string]domain.ImagePreview{}}, nil
		}
		return nil, err
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &imageManifest{Version: 1, Images: map[string]domain.ImagePreview{}}, nil
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return &imageManifest{Version: 1, Images: map[string]domain.ImagePreview{}}, nil
	}
	var manifest imageManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, err
	}
	if manifest.Images == nil {
		manifest.Images = make(map[string]domain.ImagePreview)
	}
	if manifest.Version == 0 {
		manifest.Version = 1
	}
	return &manifest, nil
}

func writeImageManifestFile(rootPath string, manifest *imageManifest) error {
	resolved, _, err := security.ResolveForWrite(rootPath, imageManifestPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	temp := resolved + ".tmp"
	if err := os.WriteFile(temp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(temp, resolved)
}

func upsertImageManifestEntry(rootPath, rel string, preview domain.ImagePreview) error {
	manifest, err := readImageManifestFile(rootPath)
	if err != nil {
		return err
	}
	if manifest.Images == nil {
		manifest.Images = make(map[string]domain.ImagePreview)
	}
	manifest.Version = 1
	manifest.Images[rel] = preview
	return writeImageManifestFile(rootPath, manifest)
}

func imagePreviewForRel(manifest *imageManifest, rel string) *domain.ImagePreview {
	if manifest == nil || manifest.Images == nil {
		return nil
	}
	preview, ok := manifest.Images[filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))]
	if !ok || strings.TrimSpace(preview.PreviewURL) == "" {
		return nil
	}
	if preview.Path == "" {
		preview.Path = filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	}
	return &preview
}

func imagePreviewForData(manifest *imageManifest, rel string, data []byte) *domain.ImagePreview {
	preview := imagePreviewForRel(manifest, rel)
	if preview == nil {
		return nil
	}
	if preview.SHA256 != "" && !strings.EqualFold(preview.SHA256, hashBytes(data)) {
		return nil
	}
	return preview
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

type positionedEdit struct {
	start int
	end   int
	index int
	text  string
}

func applyTextEdits(content string, edits []TextEdit) (string, error) {
	if len(edits) == 0 {
		return "", domain.ErrInvalidEdit
	}

	positioned := make([]positionedEdit, 0, len(edits))
	for index, edit := range edits {
		endLine := edit.EndLine
		if endLine <= 0 {
			endLine = edit.StartLine
		}
		endColumn := edit.EndColumn
		if endColumn <= 0 {
			endColumn = edit.StartColumn
		}

		start, err := textOffset(content, edit.StartLine, edit.StartColumn)
		if err != nil {
			return "", err
		}
		end, err := textOffset(content, endLine, endColumn)
		if err != nil {
			return "", err
		}
		if start > end {
			return "", domain.ErrInvalidEdit
		}

		positioned = append(positioned, positionedEdit{
			start: start,
			end:   end,
			index: index,
			text:  edit.Text,
		})
	}

	sort.Slice(positioned, func(i, j int) bool {
		if positioned[i].start == positioned[j].start {
			return positioned[i].index > positioned[j].index
		}
		return positioned[i].start > positioned[j].start
	})

	updated := content
	nextStart := len(content)
	for _, edit := range positioned {
		if edit.end > nextStart {
			return "", domain.ErrInvalidEdit
		}
		updated = updated[:edit.start] + edit.text + updated[edit.end:]
		nextStart = edit.start
	}
	return updated, nil
}

func textOffset(content string, line, column int) (int, error) {
	if line <= 0 || column <= 0 {
		return 0, domain.ErrInvalidEdit
	}

	start, end, ok := lineBounds(content, line)
	if !ok {
		return 0, domain.ErrInvalidEdit
	}

	targetRunes := column - 1
	seenRunes := 0
	for byteOffset := range content[start:end] {
		if seenRunes == targetRunes {
			return start + byteOffset, nil
		}
		seenRunes++
	}
	if seenRunes == targetRunes {
		return end, nil
	}
	return 0, domain.ErrInvalidEdit
}

func lineBounds(content string, targetLine int) (int, int, bool) {
	line := 1
	start := 0
	for index := 0; index < len(content); index++ {
		if content[index] != '\n' {
			continue
		}
		if line == targetLine {
			return start, index, true
		}
		line++
		start = index + 1
	}
	if line == targetLine {
		return start, len(content), true
	}
	return 0, 0, false
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
