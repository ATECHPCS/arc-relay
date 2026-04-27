package sync

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestMemoryWatcher_RunOnceIngestsDelta(t *testing.T) {
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "claude-projects", "-Users-ian")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	jsonlPath := filepath.Join(projectDir, "abc.jsonl")
	transcript := `{"type":"user","uuid":"u1","timestamp":"t","message":{"role":"user","content":"first"}}` + "\n"
	if err := os.WriteFile(jsonlPath, []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}

	var (
		mu       sync.Mutex
		received [][]byte
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		received = append(received, body)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages_added": 1, "events_added": 0, "bytes_seen": int64(len(body)),
		})
	}))
	defer server.Close()

	w := &MemoryWatcher{
		BaseURL:    server.URL,
		APIKey:     "test",
		RootDir:    filepath.Join(dir, "claude-projects"),
		StatePath:  filepath.Join(dir, "state.json"),
		HTTPClient: server.Client(),
	}
	if err := w.RunOnce(); err != nil {
		t.Fatalf("run once: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("want 1 POST, got %d", len(received))
	}

	// Decode and verify the body shape
	var got ingestRequest
	if err := json.Unmarshal(received[0], &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got.SessionID != "abc" {
		t.Fatalf("want session_id=abc, got %q", got.SessionID)
	}
	if got.Platform != "claude-code" {
		t.Fatalf("want platform=claude-code, got %q", got.Platform)
	}
	if string(got.JSONL) != transcript {
		t.Fatalf("jsonl body mismatch: %q", got.JSONL)
	}

	// Verify watermark file was written
	stateData, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("reading state: %v", err)
	}
	var st stateFile
	if err := json.Unmarshal(stateData, &st); err != nil {
		t.Fatalf("parsing state: %v", err)
	}
	fs := st.Files[jsonlPath]
	if fs == nil || fs.BytesSeen == 0 {
		t.Fatalf("watermark not written after successful POST")
	}
}

func TestMemoryWatcher_NoOpOnNoChange(t *testing.T) {
	dir := t.TempDir()
	pd := filepath.Join(dir, "p", "-x")
	_ = os.MkdirAll(pd, 0o755)
	_ = os.WriteFile(filepath.Join(pd, "s1.jsonl"), []byte("{}\n"), 0o644)

	var posts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages_added": 0, "events_added": 0, "bytes_seen": 3,
		})
	}))
	defer server.Close()

	w := &MemoryWatcher{
		BaseURL:    server.URL,
		APIKey:     "k",
		RootDir:    filepath.Join(dir, "p"),
		StatePath:  filepath.Join(dir, "state.json"),
		HTTPClient: server.Client(),
	}
	_ = w.RunOnce()
	_ = w.RunOnce() // second call should be a no-op — watermark already at file size
	if posts != 1 {
		t.Fatalf("want 1 POST, got %d (no-op detection broken)", posts)
	}
}

func TestMemoryWatcher_HTTPFailureDoesNotAdvanceWatermark(t *testing.T) {
	dir := t.TempDir()
	pd := filepath.Join(dir, "p", "-x")
	_ = os.MkdirAll(pd, 0o755)
	_ = os.WriteFile(filepath.Join(pd, "s1.jsonl"), []byte("{}\n"), 0o644)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	w := &MemoryWatcher{
		BaseURL:    server.URL,
		APIKey:     "k",
		RootDir:    filepath.Join(dir, "p"),
		StatePath:  filepath.Join(dir, "state.json"),
		HTTPClient: server.Client(),
	}
	_ = w.RunOnce()

	// Watermark should NOT have advanced
	st := w.loadState()
	for path, fs := range st.Files {
		if fs.BytesSeen != 0 {
			t.Fatalf("watermark advanced on failure: %s = %d", path, fs.BytesSeen)
		}
	}
}

func TestMemoryWatcher_CorruptStateFileResetsCleanly(t *testing.T) {
	dir := t.TempDir()
	pd := filepath.Join(dir, "p", "-x")
	_ = os.MkdirAll(pd, 0o755)
	_ = os.WriteFile(filepath.Join(pd, "s1.jsonl"), []byte("{}\n"), 0o644)

	statePath := filepath.Join(dir, "state.json")
	// Pre-write a corrupted state file
	_ = os.WriteFile(statePath, []byte("{not json"), 0o600)

	var posts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages_added": 0, "events_added": 0, "bytes_seen": 3,
		})
	}))
	defer server.Close()

	w := &MemoryWatcher{
		BaseURL:    server.URL,
		APIKey:     "k",
		RootDir:    filepath.Join(dir, "p"),
		StatePath:  statePath,
		HTTPClient: server.Client(),
	}
	if err := w.RunOnce(); err != nil {
		t.Fatalf("run once: %v", err)
	}
	// We don't assert post count beyond "no crash" — the test's value is
	// proving the watcher recovers from corrupt state without panicking.
	if posts < 1 {
		t.Fatalf("expected at least 1 POST after recovery, got %d", posts)
	}
}

func TestMemoryWatcher_DecodesProjectDir(t *testing.T) {
	cases := []struct {
		escaped, want string
	}{
		{"-Users-ian", "/Users/ian"},
		{"-Users-ian-code-arc-relay", "/Users/ian/code/arc/relay"},
		{"-tmp-test", "/tmp/test"},
	}
	for _, c := range cases {
		got := decodeProjectDir(c.escaped)
		if got != c.want {
			t.Errorf("decodeProjectDir(%q) = %q, want %q", c.escaped, got, c.want)
		}
	}
}
