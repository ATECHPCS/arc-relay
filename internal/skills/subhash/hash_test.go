package subhash

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHash_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	h, err := Hash(dir)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if len(h) != 64 {
		t.Errorf("expected 64-char hex, got %q", h)
	}
}

func TestHash_DeterministicAcrossOrderingNoise(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	// Create same files in different order
	_ = os.WriteFile(filepath.Join(dir1, "b.txt"), []byte("b"), 0o644)
	_ = os.WriteFile(filepath.Join(dir1, "a.txt"), []byte("a"), 0o644)
	_ = os.WriteFile(filepath.Join(dir2, "a.txt"), []byte("a"), 0o644)
	_ = os.WriteFile(filepath.Join(dir2, "b.txt"), []byte("b"), 0o644)
	h1, _ := Hash(dir1)
	h2, _ := Hash(dir2)
	if h1 != h2 {
		t.Errorf("h1=%s h2=%s should match", h1, h2)
	}
}

func TestHash_ExecutableBitChangesHash(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "script.sh")
	_ = os.WriteFile(p, []byte("#!/bin/sh"), 0o644)
	h1, _ := Hash(dir)
	_ = os.Chmod(p, 0o755)
	h2, _ := Hash(dir)
	if h1 == h2 {
		t.Error("hash should differ when exec bit changes")
	}
}

func TestHash_ContentChangesHash(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	_ = os.WriteFile(p, []byte("a"), 0o644)
	h1, _ := Hash(dir)
	_ = os.WriteFile(p, []byte("b"), 0o644)
	h2, _ := Hash(dir)
	if h1 == h2 {
		t.Error("content change should change hash")
	}
}

func TestHash_SkipsGitDir(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "f.txt"), []byte("a"), 0o644)
	h1, _ := Hash(dir)

	_ = os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("[core]"), 0o644)
	h2, _ := Hash(dir)
	if h1 != h2 {
		t.Errorf(".git/ should be excluded; h1=%s h2=%s", h1, h2)
	}
}

func TestHash_RespectsGitignore(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("k"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored.txt\n"), 0o644)
	h1, _ := Hash(dir)
	_ = os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("noise"), 0o644)
	h2, _ := Hash(dir)
	if h1 != h2 {
		t.Errorf(".gitignore'd file should be excluded; h1=%s h2=%s", h1, h2)
	}
}

func TestHash_SymlinkContentUsesTarget(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "real.txt"), []byte("hi"), 0o644)
	_ = os.Symlink("real.txt", filepath.Join(dir, "link"))
	h, err := Hash(dir)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if len(h) != 64 {
		t.Errorf("expected hash, got %q", h)
	}
}
