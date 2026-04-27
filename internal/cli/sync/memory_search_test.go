package sync

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) (*MemorySearchClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &MemorySearchClient{
		BaseURL:    srv.URL,
		APIKey:     "test-key",
		HTTPClient: srv.Client(),
	}, srv
}

func TestMemorySearchClient_Search(t *testing.T) {
	var capturedURL *url.URL
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		capturedURL = r.URL
		_ = json.NewEncoder(w).Encode(map[string]any{
			"hits": []map[string]any{
				{"session_id": "s1", "role": "user", "timestamp": "t",
					"snippet": "BM25 ranking is great", "score": -1.2},
			},
			"banner": researchOnlyBanner,
		})
	})
	out, err := c.Search("BM25", SearchOptions{Limit: 5})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !strings.Contains(out, "BM25 ranking is great") {
		t.Fatalf("missing hit content: %q", out)
	}
	if !strings.Contains(out, "RESEARCH ONLY") {
		t.Fatalf("missing banner: %q", out)
	}
	if capturedURL == nil || capturedURL.Query().Get("q") != "BM25" {
		t.Fatalf("expected q=BM25, got %v", capturedURL)
	}
	if capturedURL.Query().Get("limit") != "5" {
		t.Fatalf("expected limit=5, got %v", capturedURL.Query().Get("limit"))
	}
}

func TestMemorySearchClient_SearchJSON(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"hits":   []any{},
			"banner": researchOnlyBanner,
		})
	})
	out, err := c.Search("anything", SearchOptions{JSON: true})
	if err != nil {
		t.Fatalf("search json: %v", err)
	}
	// JSON mode returns the wire payload verbatim — must parse cleanly.
	var parsed map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &parsed); err != nil {
		t.Fatalf("json mode output not valid JSON: %v\n%s", err, out)
	}
	if _, ok := parsed["hits"]; !ok {
		t.Fatalf("expected hits key, got %v", parsed)
	}
}

func TestMemorySearchClient_List(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		// The relay encodes []*store.MemorySession with no JSON tags,
		// so field names are Go PascalCase (SessionID, ProjectDir, etc.).
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sessions": []map[string]any{
				{"SessionID": "abc", "ProjectDir": "/p", "FilePath": "/f1", "Platform": "claude-code"},
				{"SessionID": "def", "ProjectDir": "/q", "FilePath": "/f2", "Platform": "claude-code"},
			},
			"banner": researchOnlyBanner,
		})
	})
	out, err := c.List(ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, sid := range []string{"abc", "def"} {
		if !strings.Contains(out, sid) {
			t.Fatalf("missing session %q in: %q", sid, out)
		}
	}
	if !strings.Contains(out, "RESEARCH ONLY") {
		t.Fatalf("missing banner")
	}
}

func TestMemorySearchClient_Stats(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"db_bytes":       1234567,
			"sessions":       42,
			"messages":       9876,
			"last_ingest_at": 0,
			"platforms":      []string{"claude-code"},
		})
	})
	out, err := c.Stats()
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	for _, expect := range []string{"Database", "Sessions", "Messages", "claude-code"} {
		if !strings.Contains(out, expect) {
			t.Fatalf("missing %q in stats output: %q", expect, out)
		}
	}
}

func TestMemorySearchClient_Show(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		// The relay encodes []*store.Message with no JSON tags,
		// so field names are Go PascalCase (Role, Timestamp, Content, etc.).
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages": []map[string]any{
				{"Role": "user", "Timestamp": "t1", "Content": "hello"},
				{"Role": "assistant", "Timestamp": "t2", "Content": "hi"},
				{"Role": "user", "Timestamp": "t3", "Content": "cool"},
			},
			"banner": researchOnlyBanner,
		})
	})
	out, err := c.Show("abc", ShowOptions{})
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	for _, expect := range []string{"hello", "hi", "cool", "RESEARCH ONLY"} {
		if !strings.Contains(out, expect) {
			t.Fatalf("missing %q in show output: %q", expect, out)
		}
	}
}

func TestMemorySearchClient_HTTPError(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	_, err := c.Search("anything", SearchOptions{})
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error missing status or body: %v", err)
	}
}
