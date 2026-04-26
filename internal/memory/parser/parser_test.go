package parser

import (
	"os"
	"slices"
	"strings"
	"testing"
)

func TestParserRegistry_ClaudeCode(t *testing.T) {
	p := Get("claude-code")
	if p == nil {
		t.Fatal("claude-code parser not registered")
	}
	if p.Platform() != "claude-code" {
		t.Fatalf("Platform() = %q, want claude-code", p.Platform())
	}
	platforms := Platforms()
	if !slices.Contains(platforms, "claude-code") {
		t.Fatalf("Platforms() does not include claude-code: %v", platforms)
	}
	if Get("nonexistent") != nil {
		t.Fatal("Get(nonexistent) should return nil")
	}
}

func TestParseClaudeCodeJSONL(t *testing.T) {
	f, err := os.Open("testdata/claudecode_sample.jsonl")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	p := Get("claude-code")
	msgs, _, err := p.Parse(f)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("expected at least one message")
	}
	roles := map[string]bool{}
	for _, m := range msgs {
		roles[m.Role] = true
	}
	if !roles["user"] && !roles["assistant"] {
		t.Fatalf("missing user/assistant roles: %v", roles)
	}
}

func TestParseClaudeCodeJSONL_SlashCommandCollapse(t *testing.T) {
	in := strings.NewReader(
		`{"type":"user","uuid":"u1","timestamp":"t","message":{"role":"user","content":"<command-name>recall</command-name><command-args>foo bar</command-args>"}}` + "\n",
	)
	p := Get("claude-code")
	msgs, _, err := p.Parse(in)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
	if !strings.HasPrefix(msgs[0].Content, "[SLASH-COMMAND: /recall") {
		t.Fatalf("slash collapse failed: %q", msgs[0].Content)
	}
}

func TestParseClaudeCodeJSONL_SkipMalformed(t *testing.T) {
	in := strings.NewReader(
		"not json\n" +
			`{"type":"user","uuid":"u1","timestamp":"t","message":{"role":"user","content":"hello"}}` + "\n",
	)
	p := Get("claude-code")
	msgs, _, err := p.Parse(in)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("want 1 valid msg, got %d", len(msgs))
	}
	if msgs[0].Content != "hello" {
		t.Fatalf("content not preserved: %q", msgs[0].Content)
	}
}
