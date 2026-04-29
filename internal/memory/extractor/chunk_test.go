package extractor

import (
	"strings"
	"testing"

	"github.com/comma-compliance/arc-relay/internal/store"
)

func TestRender(t *testing.T) {
	m := &store.Message{
		Role:      "user",
		Timestamp: "2026-04-28T19:59:00Z",
		Content:   "  some content with leading/trailing space  \n",
	}
	got := Render(m)
	want := "[USER 2026-04-28T19:59:00Z] some content with leading/trailing space\n\n"
	if got != want {
		t.Errorf("Render mismatch:\n got %q\nwant %q", got, want)
	}
}

func TestChunk_NeverSplitsAMessage(t *testing.T) {
	msgs := []*store.Message{
		{UUID: "a", Role: "user", Timestamp: "t1", Content: strings.Repeat("x", 100)},
		{UUID: "b", Role: "assistant", Timestamp: "t2", Content: strings.Repeat("y", 100)},
		{UUID: "c", Role: "user", Timestamp: "t3", Content: strings.Repeat("z", 100)},
	}
	chunks := ChunkMessages(msgs, 250)
	for _, c := range chunks {
		// Every chunk's Text should be the concatenation of fully rendered messages.
		// A clean way to check: every UUID in Text appears via Render() exactly once.
		// Here, just check chars >= sum of rendered lengths is impossible — chunks
		// are always exact rendered concatenations.
		if c.Chars != len(c.Text) {
			t.Errorf("Chars/len mismatch: chars=%d len=%d", c.Chars, len(c.Text))
		}
	}
	// Total UUIDs in chunks should equal total input messages
	totalUUIDs := 0
	for _, c := range chunks {
		totalUUIDs += len(c.UUIDs)
	}
	if totalUUIDs != len(msgs) {
		t.Errorf("UUID count: got %d want %d", totalUUIDs, len(msgs))
	}
}

func TestChunk_OversizeMessageIsItsOwnChunk(t *testing.T) {
	big := strings.Repeat("a", 6000)
	msgs := []*store.Message{
		{UUID: "small1", Role: "user", Timestamp: "t1", Content: "small"},
		{UUID: "huge", Role: "assistant", Timestamp: "t2", Content: big},
		{UUID: "small2", Role: "user", Timestamp: "t3", Content: "tiny"},
	}
	chunks := ChunkMessages(msgs, 5000)
	// Find the chunk containing "huge"
	for _, c := range chunks {
		for _, u := range c.UUIDs {
			if u == "huge" {
				if len(c.UUIDs) != 1 {
					t.Errorf("huge message should be alone in chunk, got %d UUIDs: %v",
						len(c.UUIDs), c.UUIDs)
				}
				return
			}
		}
	}
	t.Fatalf("huge message not found in any chunk")
}

func TestChunk_EmptyInputReturnsNil(t *testing.T) {
	if got := ChunkMessages(nil, 5000); got != nil {
		t.Errorf("ChunkMessages(nil): got %v want nil", got)
	}
	if got := ChunkMessages([]*store.Message{}, 5000); got != nil {
		t.Errorf("ChunkMessages(empty): got %v want nil", got)
	}
}

func TestChunk_TargetZeroDefaultsTo5000(t *testing.T) {
	// 4 small messages should fit in one chunk under default 5000 target.
	var msgs []*store.Message
	for i := 0; i < 4; i++ {
		msgs = append(msgs, &store.Message{UUID: "u", Role: "user", Timestamp: "t",
			Content: strings.Repeat("a", 100)})
	}
	chunks := ChunkMessages(msgs, 0)
	if len(chunks) != 1 {
		t.Errorf("expected 1 chunk under default target, got %d", len(chunks))
	}
}
