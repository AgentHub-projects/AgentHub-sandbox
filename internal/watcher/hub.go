package watcher

import (
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Event struct {
	AgentID    string    `json:"agentId"`
	Path       string    `json:"path"`
	ChangeType string    `json:"changeType"`
	Mtime      time.Time `json:"mtime"`
	Version    string    `json:"version,omitempty"`
	Actor      string    `json:"actor"`
}

type subscriber struct {
	agentID string
	paths   map[string]struct{}
	ch      chan Event
}

type Hub struct {
	mu   sync.RWMutex
	subs map[string]*subscriber
}

func NewHub() *Hub {
	return &Hub{
		subs: make(map[string]*subscriber),
	}
}

func (h *Hub) Subscribe(agentID, subscriberID string) (<-chan Event, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()

	sub := &subscriber{
		agentID: agentID,
		paths:   make(map[string]struct{}),
		ch:      make(chan Event, 64),
	}
	h.subs[subscriberID] = sub

	return sub.ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if existing := h.subs[subscriberID]; existing != nil {
			delete(h.subs, subscriberID)
			close(existing.ch)
		}
	}
}

func (h *Hub) SetPaths(subscriberID string, paths []string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()

	sub := h.subs[subscriberID]
	if sub == nil {
		return nil
	}
	sub.paths = make(map[string]struct{}, len(paths))
	normalized := make([]string, 0, len(paths))
	for _, item := range paths {
		path := normalizePath(item)
		sub.paths[path] = struct{}{}
		normalized = append(normalized, path)
	}
	return normalized
}

func (h *Hub) Broadcast(event Event) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for _, sub := range h.subs {
		if sub.agentID != event.AgentID {
			continue
		}
		if !matches(sub.paths, event.Path) {
			continue
		}
		select {
		case sub.ch <- event:
		default:
		}
	}
}

func matches(paths map[string]struct{}, changed string) bool {
	if len(paths) == 0 {
		return false
	}
	changed = normalizePath(changed)
	for watchPath := range paths {
		if watchPath == "." {
			return true
		}
		if changed == watchPath {
			return true
		}
		if strings.HasPrefix(changed, watchPath+"/") {
			return true
		}
	}
	return false
}

func normalizePath(raw string) string {
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(raw)))
	if cleaned == "" {
		return "."
	}
	return cleaned
}
