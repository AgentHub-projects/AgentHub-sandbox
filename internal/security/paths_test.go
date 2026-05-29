package security

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"agenthub-sandbox/internal/domain"
)

func TestNormalizeRelativePath(t *testing.T) {
	abs, err := filepath.Abs("example.txt")
	if err != nil {
		t.Fatalf("resolve absolute path: %v", err)
	}

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr error
	}{
		{name: "empty becomes dot", input: "", want: "."},
		{name: "normal path", input: "dir/file.txt", want: "dir/file.txt"},
		{name: "clean path", input: "dir/../file.txt", want: "file.txt"},
		{name: "parent traversal", input: "../file.txt", wantErr: domain.ErrPathEscapesWorktree},
		{name: "absolute path", input: abs, wantErr: domain.ErrInvalidPath},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeRelativePath(tc.input)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("expected error %v, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalize path: %v", err)
			}
			if got != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
	}
}

func TestResolveForWrite(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatalf("create nested dir: %v", err)
	}

	resolved, rel, err := ResolveForWrite(root, "nested/file.txt")
	if err != nil {
		t.Fatalf("resolve for write: %v", err)
	}

	if rel != "nested/file.txt" {
		t.Fatalf("expected relative path nested/file.txt, got %s", rel)
	}

	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve temp root: %v", err)
	}
	want := filepath.Join(rootReal, "nested", "file.txt")
	if filepath.Clean(resolved) != filepath.Clean(want) {
		t.Fatalf("expected resolved path %s, got %s", want, resolved)
	}

	if _, _, err := ResolveForWrite(root, "../escape.txt"); !errors.Is(err, domain.ErrPathEscapesWorktree) {
		t.Fatalf("expected escape error, got %v", err)
	}
}
