package project

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectProjectDirWithMCPJSON(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte("{}"), 0644)

	got, err := DetectProjectDir(dir)
	if err != nil {
		t.Fatalf("DetectProjectDir: %v", err)
	}
	if got != dir {
		t.Errorf("got %q, want %q", got, dir)
	}
}

func TestDetectProjectDirWithClaudeDir(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, ".claude"), 0755)

	got, err := DetectProjectDir(dir)
	if err != nil {
		t.Fatalf("DetectProjectDir: %v", err)
	}
	if got != dir {
		t.Errorf("got %q, want %q", got, dir)
	}
}

func TestDetectProjectDirWalksUp(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, ".mcp.json"), []byte("{}"), 0644)
	sub := filepath.Join(root, "src", "components")
	os.MkdirAll(sub, 0755)

	got, err := DetectProjectDir(sub)
	if err != nil {
		t.Fatalf("DetectProjectDir: %v", err)
	}
	if got != root {
		t.Errorf("got %q, want %q", got, root)
	}
}

func TestDetectProjectDirNoMarker(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "some", "nested", "dir")
	os.MkdirAll(sub, 0755)

	got, err := DetectProjectDir(sub)
	if err != nil {
		t.Fatalf("DetectProjectDir: %v", err)
	}
	// Should return the starting dir when no marker found
	if got != sub {
		t.Errorf("got %q, want %q (should return start dir when no marker)", got, sub)
	}
}

func TestDetectProjectDirWithGitDir(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, ".git"), 0755)

	got, err := DetectProjectDir(dir)
	if err != nil {
		t.Fatalf("DetectProjectDir: %v", err)
	}
	if got != dir {
		t.Errorf("got %q, want %q", got, dir)
	}
}

func TestIsHomeDir(t *testing.T) {
	// Use filepath.Join to build platform-correct paths
	tests := []struct {
		dir  string
		want bool
	}{
		{filepath.Join("/", "home", "user"), true},
		{filepath.Join("/", "home", "jchurch"), true},
		{filepath.Join("/", "Users", "jchurch"), true},
		{filepath.Join("/", "mnt", "c", "Users", "jerem"), true},
		{filepath.Join("/", "home", "user", "projects", "myapp"), false},
		{filepath.Join("/", "tmp", "test"), false},
		{filepath.Join("/", "var", "lib", "something"), false},
	}

	// On Windows, also test native Windows paths
	if os.PathSeparator == '\\' {
		tests = append(tests,
			struct {
				dir  string
				want bool
			}{`C:\Users\jerem`, true},
			struct {
				dir  string
				want bool
			}{`C:\Users\jerem\projects\app`, false},
		)
	}

	for _, tt := range tests {
		got := isHomeDir(tt.dir)
		if got != tt.want {
			t.Errorf("isHomeDir(%q) = %v, want %v", tt.dir, got, tt.want)
		}
	}
}

func TestDetectTargets(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte("{}"), 0644)

	all := AllTargets()
	detected := DetectTargets(dir, all)

	if len(detected) != 1 {
		t.Fatalf("expected 1 detected target, got %d", len(detected))
	}
	if detected[0].Name() != "claude-code" {
		t.Errorf("expected claude-code target, got %q", detected[0].Name())
	}
}

func TestDetectTargetsNoneFound(t *testing.T) {
	dir := t.TempDir()

	all := AllTargets()
	detected := DetectTargets(dir, all)

	if len(detected) != 0 {
		t.Errorf("expected 0 detected targets, got %d", len(detected))
	}
}
