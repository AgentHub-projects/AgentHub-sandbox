package worktree

import (
	"testing"
	"time"
)

func TestRegistryLifecycle(t *testing.T) {
	registry := NewRegistry()
	state := &State{
		AgentID:       "leader",
		BranchName:    "agent/leader",
		RootPath:      "/tmp/leader",
		HeadSHA:       "abc123",
		PreparedAt:    time.Now().UTC(),
		ActiveExecIDs: map[string]struct{}{"exec-1": {}},
	}

	registry.Upsert(state)

	got, ok := registry.Get("leader")
	if !ok {
		t.Fatalf("expected state to exist")
	}
	if got.AgentID != "leader" || got.BranchName != "agent/leader" {
		t.Fatalf("unexpected state: %+v", got)
	}

	got.ActiveExecIDs["exec-2"] = struct{}{}
	fresh, _ := registry.Get("leader")
	if _, ok := fresh.ActiveExecIDs["exec-2"]; ok {
		t.Fatalf("registry returned shared map instead of a clone")
	}

	if err := registry.RegisterExec("leader", "exec-2"); err != nil {
		t.Fatalf("register exec: %v", err)
	}
	fresh, _ = registry.Get("leader")
	if _, ok := fresh.ActiveExecIDs["exec-2"]; !ok {
		t.Fatalf("expected exec to be registered")
	}

	registry.UnregisterExec("leader", "exec-1")
	fresh, _ = registry.Get("leader")
	if _, ok := fresh.ActiveExecIDs["exec-1"]; ok {
		t.Fatalf("expected exec to be removed")
	}

	if _, ok := registry.Delete("leader"); !ok {
		t.Fatalf("expected delete to remove state")
	}
	if _, ok := registry.Get("leader"); ok {
		t.Fatalf("expected registry to be empty after delete")
	}
}
