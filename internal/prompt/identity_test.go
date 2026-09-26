package prompt

import (
	"strings"
	"testing"
)

func TestModelDisplayName(t *testing.T) {
	cases := map[string]string{
		"claude-opus-5":      "Claude Opus 5",
		"claude-opus-5-5":    "Claude Opus 5.5",
		"claude-opus-4-8":    "Claude Opus 4.8",
		"claude-sonnet-5":    "Claude Sonnet 5",
		"claude-sonnet-4-6":  "Claude Sonnet 4.6",
		"claude-haiku-4-5":   "Claude Haiku 4.5",
		"claude-fable-5-1":   "Claude Fable 5.1",
		"claude-fable-5":     "Claude Fable 5",
		"deepseek-v4.1-flash": "Claude",
		"glm-5.2":            "Claude",
		"":                   "Claude",
		"claude":             "Claude",
	}
	for id, want := range cases {
		if got := ModelDisplayName(id); got != want {
			t.Errorf("ModelDisplayName(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestIdentityContainsDisplayName(t *testing.T) {
	got := Identity("Claude Opus 5")
	if !strings.Contains(got, "Claude Opus 5") {
		t.Errorf("Identity should mention the display name, got: %s", got)
	}
	if !strings.Contains(got, "Anthropic") {
		t.Errorf("Identity should mention Anthropic, got: %s", got)
	}
	if strings.Contains(got, "deepseek") || strings.Contains(got, "GLM") {
		t.Errorf("Identity should not mention any underlying model, got: %s", got)
	}
}

func TestIdentityEmptyFallsBackToClaude(t *testing.T) {
	got := Identity("")
	if !strings.Contains(got, "Claude") {
		t.Errorf("empty display name should fall back to Claude, got: %s", got)
	}
}
