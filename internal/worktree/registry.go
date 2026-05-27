package worktree

import (
	"maps"
	"sync"
	"time"

	"agenthub-sandbox/internal/domain"
)

type State struct {
	AgentID       string
	BranchName    string
	RootPath      string
	HeadSHA       string
	PreparedAt    time.Time
	ActiveExecIDs map[string]struct{}
}

// Info 把内部状态转成对外返回用的结构。
func (s *State) Info() domain.WorktreeInfo {
	return domain.WorktreeInfo{
		AgentID:    s.AgentID,
		BranchName: s.BranchName,
		RootPath:   s.RootPath,
		HeadSHA:    s.HeadSHA,
		PreparedAt: s.PreparedAt,
	}
}

// clone 用来避免把内部 map 和状态直接暴露给外层。
func (s *State) clone() *State {
	if s == nil {
		return nil
	}
	return &State{
		AgentID:       s.AgentID,
		BranchName:    s.BranchName,
		RootPath:      s.RootPath,
		HeadSHA:       s.HeadSHA,
		PreparedAt:    s.PreparedAt,
		ActiveExecIDs: maps.Clone(s.ActiveExecIDs),
	}
}

type Registry struct {
	mu     sync.RWMutex
	states map[string]*State
}

// Registry 维护当前 sandbox 里的 agentId -> worktree 状态映射。
func NewRegistry() *Registry {
	return &Registry{
		states: make(map[string]*State),
	}
}

// Upsert 在 prepare 或状态刷新时写回最新的 worktree 信息。
func (r *Registry) Upsert(state *State) *State {
	r.mu.Lock()
	defer r.mu.Unlock()

	cloned := state.clone()
	if cloned.ActiveExecIDs == nil {
		cloned.ActiveExecIDs = make(map[string]struct{})
	}
	r.states[cloned.AgentID] = cloned
	return cloned.clone()
}

func (r *Registry) Get(agentID string) (*State, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	state, ok := r.states[agentID]
	return state.clone(), ok
}

// MustGet 用在必须先 prepare 才能继续的调用链上。
func (r *Registry) MustGet(agentID string) (*State, error) {
	state, ok := r.Get(agentID)
	if !ok {
		return nil, domain.ErrWorktreeNotPrepared
	}
	return state, nil
}

func (r *Registry) Delete(agentID string) (*State, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	state, ok := r.states[agentID]
	if ok {
		delete(r.states, agentID)
	}
	return state.clone(), ok
}

func (r *Registry) List() []*State {
	r.mu.RLock()
	defer r.mu.RUnlock()

	items := make([]*State, 0, len(r.states))
	for _, state := range r.states {
		items = append(items, state.clone())
	}
	return items
}

func (r *Registry) UpdateHead(agentID, headSHA string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if state := r.states[agentID]; state != nil {
		state.HeadSHA = headSHA
	}
}

// RegisterExec / UnregisterExec 用来追踪每个 agent 当前活跃的执行任务。
func (r *Registry) RegisterExec(agentID, execID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	state := r.states[agentID]
	if state == nil {
		return domain.ErrWorktreeNotPrepared
	}
	if state.ActiveExecIDs == nil {
		state.ActiveExecIDs = make(map[string]struct{})
	}
	state.ActiveExecIDs[execID] = struct{}{}
	return nil
}

func (r *Registry) UnregisterExec(agentID, execID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	state := r.states[agentID]
	if state == nil || state.ActiveExecIDs == nil {
		return
	}
	delete(state.ActiveExecIDs, execID)
}
