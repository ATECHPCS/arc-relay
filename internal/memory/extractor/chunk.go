package extractor

import (
	"fmt"
	"strings"

	"github.com/comma-compliance/arc-relay/internal/store"
)

// Chunk is one mem0.add_memory call's worth of input — a slice of messages
// rendered to a single string with provenance preserved.
type Chunk struct {
	Text  string   // the rendered transcript window (what mem0 sees)
	UUIDs []string // source message UUIDs in original order
	Chars int      // utf8.RuneCountInString-ish; len(Text) is fine here
}

// Render formats one message as "[ROLE TIMESTAMP] content\n\n" with the
// content trimmed of leading/trailing whitespace. The trailing blank line
// gives the LLM a clear separator between turns.
func Render(m *store.Message) string {
	role := m.Role
	if role == "" {
		role = "?"
	}
	ts := m.Timestamp
	if ts == "" {
		ts = ""
	}
	return fmt.Sprintf("[%s %s] %s\n\n",
		strings.ToUpper(role), ts, strings.TrimSpace(m.Content))
}

// ChunkMessages groups messages into ~target-char windows WITHOUT splitting any
// single message across chunks. A chunk closes as soon as adding the next
// message would push it past target chars (so chunks are usually slightly
// smaller than target, never larger by more than one message length).
//
// Boundary preference: don't split a user→assistant adjacency mid-pair when
// possible — but never violate the "no message split" invariant to honor
// this preference.
//
// target <= 0 defaults to 5000.
func ChunkMessages(msgs []*store.Message, target int) []Chunk {
	if target <= 0 {
		target = 5000
	}
	if len(msgs) == 0 {
		return nil
	}

	var out []Chunk
	var curBuf strings.Builder
	var curUUIDs []string

	flush := func() {
		if curBuf.Len() == 0 {
			return
		}
		out = append(out, Chunk{
			Text:  curBuf.String(),
			UUIDs: append([]string(nil), curUUIDs...),
			Chars: curBuf.Len(),
		})
		curBuf.Reset()
		curUUIDs = curUUIDs[:0]
	}

	for _, m := range msgs {
		rendered := Render(m)
		// If this single message is bigger than target by itself, it gets
		// its own chunk — better than splitting mid-message.
		if len(rendered) >= target {
			flush()
			out = append(out, Chunk{
				Text:  rendered,
				UUIDs: []string{m.UUID},
				Chars: len(rendered),
			})
			continue
		}
		// If adding this message would exceed target, flush first.
		if curBuf.Len()+len(rendered) > target && curBuf.Len() > 0 {
			flush()
		}
		curBuf.WriteString(rendered)
		curUUIDs = append(curUUIDs, m.UUID)
	}
	flush()
	return out
}
