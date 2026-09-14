package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTopicCLIConfig(t *testing.T) {
	for _, item := range []struct {
		name, cli, want string
		invalid         bool
	}{
		{"default", "", "claude", false},
		{"claude", "claude", "claude", false},
		{"codex", "codex", "codex", false},
		{"invalid", "typo", "", true},
	} {
		t.Run(item.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "tgcc.toml")
			if err := os.WriteFile(path, []byte("[[group]]\nchat_id = -1\n[[group.topic]]\nthread_id = 1\ncli = \""+item.cli+"\"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadTgccToml(path)
			if item.invalid {
				if err == nil {
					t.Fatal("invalid CLI accepted")
				}
				return
			}
			if err != nil || cfg.Groups[0].Topics[0].CLI != item.want {
				t.Fatalf("%+v %v", cfg, err)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "tgcc.toml")
	if err := os.WriteFile(path, []byte("[codex]\nbin = \"/custom/codex\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := loadTOML(path, &cfg); err != nil || cfg.Codex.Bin != "/custom/codex" {
		t.Fatalf("Codex binary configuration: %+v %v", cfg.Codex, err)
	}
}

func TestModelForCLI(t *testing.T) {
	if model, err := ModelForCLI("codex", ""); err != nil || model != "gpt-5.5" {
		t.Fatalf("default: %s %v", model, err)
	}
	for _, model := range []string{"opus", "sonnet", "claude-sonnet-4-6"} {
		if _, err := ModelForCLI("codex", model); err == nil {
			t.Fatalf("accepted Claude model %s", model)
		}
	}
	if _, err := ModelForCLI("claude", "gpt-5.5"); err == nil {
		t.Fatal("accepted Codex model for Claude")
	}
}
