package parser

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"

	"github.com/comma-compliance/arc-relay/internal/store"
)

// claudeCodeParser converts ~/.claude/projects/<project>/<uuid>.jsonl into
// store.Message rows + CompactEvent records.
type claudeCodeParser struct{}

func (claudeCodeParser) Platform() string { return "claude-code" }
func (claudeCodeParser) Parse(r io.Reader) ([]*store.Message, []*CompactEvent, error) {
	return parseClaudeCodeJSONL(r)
}

func init() { Register(claudeCodeParser{}) }

// rawLine is the union of shapes Claude Code emits per JSONL line.
type rawLine struct {
	Type             string          `json:"type"`
	UUID             string          `json:"uuid"`
	ParentUUID       string          `json:"parentUuid"`
	Timestamp        string          `json:"timestamp"`
	SessionID        string          `json:"sessionId"`
	Message          json.RawMessage `json:"message"`
	TriggerType      string          `json:"triggerType"`
	TokenCountBefore int             `json:"tokenCountBefore"`
}

type rawMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type contentBlock struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input"`
	Content json.RawMessage `json:"content"`
}

// slashCmdRe collapses <command-name>...</command-name><command-args>...</command-args>
// fragments into a single literal token at parse time. This is the prompt-injection
// guard from the design spec §7 — once recall surfaces these messages, the original
// XML-ish tags are gone and an LLM cannot re-interpret them as live invocations.
var slashCmdRe = regexp.MustCompile(
	`(?s)<command-name>([^<]+)</command-name>\s*<command-args>([^<]*)</command-args>`,
)

func parseClaudeCodeJSONL(r io.Reader) ([]*store.Message, []*CompactEvent, error) {
	var msgs []*store.Message
	var events []*CompactEvent

	scanner := bufio.NewScanner(r)
	// Default scanner buffer is 64KB; tool outputs can be much larger.
	scanner.Buffer(make([]byte, 0, 1<<20), 8<<20) // 8 MiB max line

	epoch := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var raw rawLine
		if err := json.Unmarshal(line, &raw); err != nil {
			slog.Warn("memory.parser: skipping malformed line", "err", err)
			continue
		}

		switch raw.Type {
		case "compact", "compaction":
			epoch++
			events = append(events, &CompactEvent{
				UUID:             raw.UUID,
				SessionID:        raw.SessionID,
				Epoch:            epoch,
				Timestamp:        raw.Timestamp,
				TriggerType:      raw.TriggerType,
				TokenCountBefore: raw.TokenCountBefore,
			})

		case "user", "assistant":
			content, err := flattenContent(raw.Message)
			if err != nil {
				slog.Warn("memory.parser: skipping bad content",
					"uuid", raw.UUID, "err", err)
				continue
			}
			content = collapseSlashCommand(content)
			role := "user"
			if raw.Type == "assistant" {
				role = "assistant"
			}
			msgs = append(msgs, &store.Message{
				UUID:       raw.UUID,
				ParentUUID: raw.ParentUUID,
				Epoch:      epoch,
				Timestamp:  raw.Timestamp,
				Role:       role,
				Content:    content,
			})
		default:
			// Other event types (system reminders without surrounding message,
			// raw tool_use without a parent message, etc.) are intentionally ignored.
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("scan: %w", err)
	}
	return msgs, events, nil
}

// flattenContent reduces Claude Code's varied content shapes to a single string.
// Plain string → as-is.
// {role: ..., content: ...} → recurse into content.
// Array of content blocks → concatenated rendering with TOOL_USE / TOOL_RESULT markers.
func flattenContent(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	// Plain string content
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	// Object with role+content (rawMessage shape)
	var rm rawMessage
	if err := json.Unmarshal(raw, &rm); err == nil && len(rm.Content) > 0 {
		return flattenContent(rm.Content)
	}
	// Array of content blocks
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, blk := range blocks {
		switch blk.Type {
		case "text":
			b.WriteString(blk.Text)
		case "tool_use":
			fmt.Fprintf(&b, "\n[TOOL_USE:%s] %s\n", blk.Name, string(blk.Input))
		case "tool_result":
			fmt.Fprintf(&b, "\n[TOOL_RESULT] %s\n", string(blk.Content))
		}
	}
	return b.String(), nil
}

// collapseSlashCommand replaces matched slash-command tags with a single
// literal token, defanging inline invocations before storage. Prefix/suffix
// content around the tags is preserved.  See §7 of the design spec.
func collapseSlashCommand(content string) string {
	return slashCmdRe.ReplaceAllStringFunc(content, func(match string) string {
		m := slashCmdRe.FindStringSubmatch(match)
		name := strings.TrimSpace(m[1])
		args := strings.TrimSpace(m[2])
		return fmt.Sprintf("[SLASH-COMMAND: /%s args=%q]", name, args)
	})
}
