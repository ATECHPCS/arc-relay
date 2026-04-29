// Package sync implements the local-side of arc-sync.
//
// MemoryWatcher tails Claude Code transcript files (~/.claude/projects/**/*.jsonl)
// and POSTs deltas to the relay's /api/memory/ingest endpoint. Runs as a launchd
// (macOS) or systemd (Linux) user service via `arc-sync memory install-service`.
package sync

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// MemoryWatcher walks RootDir for *.jsonl files, POSTs new bytes to BaseURL,
// and persists per-file watermarks in StatePath. Long-running mode uses
// fsnotify with a 5s poll fallback if fsnotify init fails, and a 30s
// belt-and-braces tick regardless.
type MemoryWatcher struct {
	BaseURL    string
	APIKey     string
	RootDir    string
	StatePath  string
	FlagPath   string // mtime change here triggers an immediate scan (Stop hook signal — Task 10)
	HTTPClient *http.Client

	// QuiescenceWindow is the silent period after a successful ingest that
	// signals "session ended" — at which point we POST /api/memory/extract
	// for that session. Zero (default) disables the extract trigger; the
	// cron backstop on the relay still picks them up eventually.
	QuiescenceWindow time.Duration

	mu sync.Mutex
	// quiescenceTimers maps session_id → pending timer. Reset on every
	// ingest; fires PostExtract when the silence threshold is reached.
	quiescenceTimers map[string]*time.Timer
}

type fileState struct {
	BytesSeen int64   `json:"bytes_seen"`
	Mtime     float64 `json:"mtime"`
}

type stateFile struct {
	Files map[string]*fileState `json:"files"`
}

// ingestRequest mirrors memory.IngestRequest on the relay side. Defined here
// (vs imported from the relay package) so arc-sync stays a pure-Go binary
// with no CGO/sqlite dependencies — duplicated wire shape, intentional.
type ingestRequest struct {
	SessionID  string  `json:"session_id"`
	ProjectDir string  `json:"project_dir"`
	FilePath   string  `json:"file_path"`
	FileMtime  float64 `json:"file_mtime"`
	BytesSeen  int64   `json:"bytes_seen"`
	Platform   string  `json:"platform"`
	JSONL      []byte  `json:"jsonl"` // base64-encoded by Go's encoding/json automatically
}

func (w *MemoryWatcher) loadState() *stateFile {
	st := &stateFile{Files: map[string]*fileState{}}
	b, err := os.ReadFile(w.StatePath)
	if err != nil {
		// File missing on first run is expected; only log other errors.
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "memory watch: cannot read state file %s: %v\n", w.StatePath, err)
		}
		return st
	}
	if err := json.Unmarshal(b, st); err != nil {
		fmt.Fprintf(os.Stderr, "memory watch: state file corrupted, starting fresh (%s): %v\n", w.StatePath, err)
		st = &stateFile{Files: map[string]*fileState{}}
	}
	if st.Files == nil {
		st.Files = map[string]*fileState{}
	}
	return st
}

func (w *MemoryWatcher) saveState(st *stateFile) error {
	if err := os.MkdirAll(filepath.Dir(w.StatePath), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(w.StatePath, b, 0o600)
}

// RunOnce performs a single full scan and returns. Used by `memory watch --once`
// and by Run() at startup for catch-up.
func (w *MemoryWatcher) RunOnce() error {
	st := w.loadState()
	return w.scan(st)
}

// Run is the long-running watch loop. fsnotify-driven with a 5s poll fallback
// if fsnotify is unavailable, and a 30s belt-and-braces tick.
func (w *MemoryWatcher) Run() error {
	st := w.loadState()
	if err := w.scan(st); err != nil {
		fmt.Fprintln(os.Stderr, "memory watch initial scan:", err)
	}

	notify, err := fsnotify.NewWatcher()
	if err != nil {
		fmt.Fprintln(os.Stderr, "memory watch: fsnotify unavailable, falling back to 5s poll:", err)
		return w.pollLoop(st)
	}
	defer notify.Close()

	if err := w.addRecursive(notify, w.RootDir); err != nil {
		return fmt.Errorf("watch root: %w", err)
	}
	// Also watch the directory containing the wakeup flag — Task 10's Stop hook
	// touches that file to signal an immediate scan. Create the dir first since
	// it's our config dir; if creation fails, log + continue (the 30s tick will
	// still catch up, just not instantly).
	if w.FlagPath != "" {
		flagDir := filepath.Dir(w.FlagPath)
		if err := os.MkdirAll(flagDir, 0o700); err != nil {
			fmt.Fprintf(os.Stderr, "memory watch: cannot create wakeup-flag dir %s: %v\n", flagDir, err)
		} else if err := notify.Add(flagDir); err != nil {
			fmt.Fprintf(os.Stderr, "memory watch: cannot watch wakeup-flag dir %s: %v\n", flagDir, err)
		}
	}

	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case ev, ok := <-notify.Events:
			if !ok {
				return nil
			}
			// Trigger scan on any .jsonl change OR a touch of the flag file.
			if !strings.HasSuffix(ev.Name, ".jsonl") && ev.Name != w.FlagPath {
				continue
			}
			if err := w.scan(st); err != nil {
				fmt.Fprintln(os.Stderr, "memory watch scan:", err)
			}
		case err, ok := <-notify.Errors:
			if !ok {
				return nil
			}
			fmt.Fprintln(os.Stderr, "memory watch fsnotify error:", err)
		case <-tick.C:
			if err := w.scan(st); err != nil {
				fmt.Fprintln(os.Stderr, "memory watch tick:", err)
			}
		}
	}
}

func (w *MemoryWatcher) addRecursive(watcher *fsnotify.Watcher, root string) error {
	return filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // best-effort; transient errors shouldn't stop the watcher
		}
		if info.IsDir() {
			_ = watcher.Add(p)
		}
		return nil
	})
}

func (w *MemoryWatcher) pollLoop(st *stateFile) error {
	for {
		if err := w.scan(st); err != nil {
			fmt.Fprintln(os.Stderr, "memory watch poll:", err)
		}
		time.Sleep(5 * time.Second)
	}
}

func (w *MemoryWatcher) scan(st *stateFile) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	return filepath.Walk(w.RootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		fs := st.Files[path]
		if fs == nil {
			fs = &fileState{}
			st.Files[path] = fs
		}
		size := info.Size()
		mtime := float64(info.ModTime().Unix())
		if size <= fs.BytesSeen && mtime <= fs.Mtime {
			return nil
		}
		delta, err := readTail(path, fs.BytesSeen)
		if err != nil {
			fmt.Fprintln(os.Stderr, "memory watch read:", err)
			return nil
		}
		if len(delta) == 0 {
			fs.Mtime = mtime
			_ = w.saveState(st)
			return nil
		}
		sessionID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		projectDir := decodeProjectDir(filepath.Base(filepath.Dir(path)))
		req := &ingestRequest{
			SessionID:  sessionID,
			ProjectDir: projectDir,
			FilePath:   path,
			FileMtime:  mtime,
			BytesSeen:  size,
			Platform:   "claude-code",
			JSONL:      delta,
		}
		body, _ := json.Marshal(req)
		resp, err := w.postIngest(body)
		if err != nil {
			fmt.Fprintln(os.Stderr, "memory watch ingest:", err)
			return nil // do NOT advance watermark; retry next scan
		}
		fs.BytesSeen = size
		fs.Mtime = mtime

		// Phase B: schedule extract POST after a quiescence window. Reset
		// the timer on every ingest for the same session; if no further
		// bytes arrive within the window, fire the extract call.
		if resp != nil && resp.MessagesAdded > 0 {
			w.scheduleQuiescenceExtract(sessionID)
		}

		return w.saveState(st)
	})
}

// scheduleQuiescenceExtract starts (or resets) a per-session timer. When
// the timer fires, we POST /api/memory/extract for that session. If
// QuiescenceWindow is 0, this is a no-op — the relay's cron loop is the
// only extraction trigger.
func (w *MemoryWatcher) scheduleQuiescenceExtract(sessionID string) {
	if w.QuiescenceWindow <= 0 {
		return
	}
	if w.quiescenceTimers == nil {
		w.quiescenceTimers = map[string]*time.Timer{}
	}
	if t, ok := w.quiescenceTimers[sessionID]; ok {
		t.Stop()
	}
	w.quiescenceTimers[sessionID] = time.AfterFunc(w.QuiescenceWindow, func() {
		// Acquire the watcher lock so we don't race with concurrent scans
		// modifying quiescenceTimers.
		w.mu.Lock()
		delete(w.quiescenceTimers, sessionID)
		w.mu.Unlock()

		if err := w.PostExtract(sessionID); err != nil {
			fmt.Fprintln(os.Stderr, "memory watch extract:", err)
			// Cron backstop on the relay will catch this on its next 30 min cycle.
		}
	})
}

// PostExtract POSTs /api/memory/extract for one session. Used by the
// quiescence trigger and by `arc-sync memory extract <session-id>`.
// Returns the relay's response decoded into ExtractResponse, or an error
// if the call failed (network, auth, 4xx/5xx).
func (w *MemoryWatcher) PostExtract(sessionID string) error {
	body, _ := json.Marshal(map[string]string{"session_id": sessionID})
	req, err := http.NewRequest("POST", w.BaseURL+"/api/memory/extract", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+w.APIKey)
	resp, err := w.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		buf, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("extract %d: %s", resp.StatusCode, strings.TrimSpace(string(buf)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// ingestResponse mirrors memory.IngestResponse on the relay side.
type ingestResponse struct {
	MessagesAdded int   `json:"messages_added"`
	EventsAdded   int   `json:"events_added"`
	BytesSeen     int64 `json:"bytes_seen"`
}

// postIngest is the renamed-and-extended replacement for the previous
// `post`. Returns the parsed ingest response so callers can decide whether
// to schedule a follow-up extraction.
func (w *MemoryWatcher) postIngest(body []byte) (*ingestResponse, error) {
	req, err := http.NewRequest("POST", w.BaseURL+"/api/memory/ingest", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+w.APIKey)
	resp, err := w.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		buf, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ingest %d: %s", resp.StatusCode, strings.TrimSpace(string(buf)))
	}
	var out ingestResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		// Don't fail the ingest on parse error — the bytes were accepted; we
		// just can't trigger quiescence-based extraction for this delta.
		return &ingestResponse{}, nil
	}
	return &out, nil
}

func readTail(path string, offset int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return nil, err
		}
	}
	return io.ReadAll(f)
}

// decodeProjectDir reverses Claude Code's `/` → `-` escaping in the project
// directory name. The transcript at `~/.claude/projects/-Users-ian-code/abc.jsonl`
// belongs to `/Users/ian/code`. Claude Code encodes `/` as `-` and the resulting
// directory name starts with a leading `-` (from the initial `/`). We strip that
// leading `-` before replacing remaining `-` characters with `/`, then prepend `/`.
//
// Caveat: project directories that legitimately contain `-` (e.g.
// `/Users/ian/my-app`) lose the original hyphens. This is a known Claude Code
// limitation — there's no round-trip-safe encoding in their format. We accept
// the lossy reverse since the value is informational (search filter), not a
// path used for filesystem access.
func decodeProjectDir(escaped string) string {
	// The leading `-` in e.g. `-Users-ian` encodes the root `/`; strip it first.
	stripped := strings.TrimPrefix(escaped, "-")
	return "/" + strings.ReplaceAll(stripped, "-", "/")
}
