package extractor

import "testing"

func TestDerive(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/Users/ian/code/arc-relay", "transcripts-arc-relay"},
		{"/Users/ian/code/arc-relay/", "transcripts-arc-relay"},
		{"/Users/ian/.claude", "transcripts-claude"},
		{"/Users/ian/Documents/Repos/ebay-template-studio", "transcripts-ebay-template-studio"},
		{"/home/foo/My Stuff", "transcripts-my-stuff"},
		{"/tmp/test_project", "transcripts-test-project"},
		{"/var/lib/docker/volumes/foo_bar/_data", "transcripts-data"}, // basename
		{"", "transcripts-unknown"},
		{".", "transcripts-unknown"},
		{"/", "transcripts-unknown"},
		{"  ", "transcripts-unknown"},
		{"---", "transcripts-unknown"},
		{"!!!", "transcripts-unknown"},
		{"UPPERCASE-Project", "transcripts-uppercase-project"},
		{"123-numeric-start", "transcripts-123-numeric-start"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := Derive(c.in)
			if got != c.want {
				t.Errorf("Derive(%q): got %q want %q", c.in, got, c.want)
			}
		})
	}
}

func TestIsTranscriptAgentID(t *testing.T) {
	cases := map[string]bool{
		"transcripts-arc-relay": true,
		"transcripts-":          true, // empty suffix still has the prefix
		"manual-foo":            false,
		"":                      false,
		"my-transcripts-foo":    false,
	}
	for in, want := range cases {
		if got := IsTranscriptAgentID(in); got != want {
			t.Errorf("IsTranscriptAgentID(%q): got %v want %v", in, got, want)
		}
	}
}
