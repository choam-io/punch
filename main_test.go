package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func setupTestDotfiles(t *testing.T) (dotfilesDir, homeDir string) {
	t.Helper()

	dotfilesDir = t.TempDir()
	homeDir = t.TempDir()

	// Override HOME so expandHome resolves to our temp dir
	t.Setenv("HOME", homeDir)

	return dotfilesDir, homeDir
}

func writeDotYaml(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dot.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestDiscoverModules(t *testing.T) {
	dotfiles, _ := setupTestDotfiles(t)

	writeDotYaml(t, filepath.Join(dotfiles, "dev", "git"), `
global:
  files:
    .gitconfig: ~/.gitconfig
`)
	writeDotYaml(t, filepath.Join(dotfiles, "terminal", "zsh"), `
files:
  .zshrc: ~/.zshrc
`)

	modules, err := discoverModules(dotfiles)
	if err != nil {
		t.Fatal(err)
	}
	if len(modules) != 2 {
		t.Fatalf("expected 2 modules, got %d", len(modules))
	}

	names := map[string]bool{}
	for _, m := range modules {
		names[m.Name] = true
	}
	if !names["dev/git"] {
		t.Error("missing dev/git module")
	}
	if !names["terminal/zsh"] {
		t.Error("missing terminal/zsh module")
	}
}

func TestDiscoverModulesInWorktree(t *testing.T) {
	dotfiles, _ := setupTestDotfiles(t)

	// Simulate a git worktree: .git is a file, not a directory
	writeFile(t, filepath.Join(dotfiles, ".git"), "gitdir: /some/other/path/.git/worktrees/branch\n")
	writeDotYaml(t, filepath.Join(dotfiles, "dev", "git"), `
files:
  .gitconfig: ~/.gitconfig
`)

	modules, err := discoverModules(dotfiles)
	if err != nil {
		t.Fatal(err)
	}
	if len(modules) != 1 {
		t.Fatalf("expected 1 module, got %d", len(modules))
	}
	if modules[0].Name != "dev/git" {
		t.Errorf("expected dev/git, got %s", modules[0].Name)
	}
}

func TestResolvedFiles(t *testing.T) {
	tests := []struct {
		name   string
		yaml   string
		expect map[string]string
	}{
		{
			name: "top-level files",
			yaml: `
files:
  .zshrc: ~/.zshrc
`,
			expect: map[string]string{".zshrc": "~/.zshrc"},
		},
		{
			name: "top-level links alias",
			yaml: `
links:
  .zshrc: ~/.zshrc
`,
			expect: map[string]string{".zshrc": "~/.zshrc"},
		},
		{
			name: "global scope",
			yaml: `
global:
  files:
    .gitconfig: ~/.gitconfig
`,
			expect: map[string]string{".gitconfig": "~/.gitconfig"},
		},
		{
			name: "global links alias",
			yaml: `
global:
  links:
    .gitconfig: ~/.gitconfig
`,
			expect: map[string]string{".gitconfig": "~/.gitconfig"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cfg DotConfig
			if err := yamlUnmarshal([]byte(tt.yaml), &cfg); err != nil {
				t.Fatal(err)
			}
			got := cfg.ResolvedFiles()
			if len(got) != len(tt.expect) {
				t.Fatalf("expected %d files, got %d: %v", len(tt.expect), len(got), got)
			}
			for k, v := range tt.expect {
				if got[k] != v {
					t.Errorf("key %s: expected %s, got %s", k, v, got[k])
				}
			}
		})
	}
}

func TestLinkCopiesFiles(t *testing.T) {
	dotfiles, home := setupTestDotfiles(t)

	// Override lockfile path
	stateOverride := t.TempDir()
	t.Setenv("PUNCH_STATE_DIR", stateOverride)

	writeFile(t, filepath.Join(dotfiles, "dev", "git", ".gitconfig"), "[user]\n\tname = test\n")
	writeDotYaml(t, filepath.Join(dotfiles, "dev", "git"), `
global:
  files:
    .gitconfig: ~/.gitconfig
`)

	err := cmdLink(dotfiles, false, false)
	if err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(home, ".gitconfig")
	got := readFile(t, target)
	if got != "[user]\n\tname = test\n" {
		t.Errorf("unexpected content: %q", got)
	}

	// Verify it's not a symlink
	info, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Error("target is a symlink, should be a copy")
	}
}

func TestLinkReplacesSymlinks(t *testing.T) {
	dotfiles, home := setupTestDotfiles(t)
	t.Setenv("PUNCH_STATE_DIR", t.TempDir())

	srcContent := "source content\n"
	writeFile(t, filepath.Join(dotfiles, "mod", "file.txt"), srcContent)
	writeDotYaml(t, filepath.Join(dotfiles, "mod"), `
files:
  file.txt: ~/.config/file.txt
`)

	// Create a symlink at the target (simulating old symlink-based install)
	target := filepath.Join(home, ".config", "file.txt")
	os.MkdirAll(filepath.Dir(target), 0o755)
	tmpFile := filepath.Join(t.TempDir(), "old-target")
	writeFile(t, tmpFile, "old symlink target\n")
	os.Symlink(tmpFile, target)

	err := cmdLink(dotfiles, false, false)
	if err != nil {
		t.Fatal(err)
	}

	// Should now be a regular file
	info, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Error("target is still a symlink after link")
	}
	got := readFile(t, target)
	if got != srcContent {
		t.Errorf("expected %q, got %q", srcContent, got)
	}
}

func TestLinkConflictDetection(t *testing.T) {
	dotfiles, home := setupTestDotfiles(t)
	t.Setenv("PUNCH_STATE_DIR", t.TempDir())

	writeFile(t, filepath.Join(dotfiles, "mod", "config"), "from source\n")
	writeDotYaml(t, filepath.Join(dotfiles, "mod"), `
files:
  config: ~/.config/app/config
`)

	// Pre-existing target with different content, no lockfile entry
	target := filepath.Join(home, ".config", "app", "config")
	writeFile(t, target, "user edited this\n")

	err := cmdLink(dotfiles, false, false)
	if err != nil {
		t.Fatal(err)
	}

	// Should NOT have overwritten -- conflict
	got := readFile(t, target)
	if got != "user edited this\n" {
		t.Errorf("conflict should have preserved target, got %q", got)
	}
}

func TestLinkForceOverwritesConflict(t *testing.T) {
	dotfiles, home := setupTestDotfiles(t)
	t.Setenv("PUNCH_STATE_DIR", t.TempDir())

	writeFile(t, filepath.Join(dotfiles, "mod", "config"), "from source\n")
	writeDotYaml(t, filepath.Join(dotfiles, "mod"), `
files:
  config: ~/.config/app/config
`)

	target := filepath.Join(home, ".config", "app", "config")
	writeFile(t, target, "user edited this\n")

	err := cmdLink(dotfiles, true, false)
	if err != nil {
		t.Fatal(err)
	}

	got := readFile(t, target)
	if got != "from source\n" {
		t.Errorf("force should have overwritten, got %q", got)
	}
}

func TestLinkSkipsUpToDate(t *testing.T) {
	dotfiles, home := setupTestDotfiles(t)
	t.Setenv("PUNCH_STATE_DIR", t.TempDir())

	content := "same content\n"
	writeFile(t, filepath.Join(dotfiles, "mod", "file"), content)
	writeDotYaml(t, filepath.Join(dotfiles, "mod"), `
files:
  file: ~/.file
`)

	// Write identical content to target
	writeFile(t, filepath.Join(home, ".file"), content)

	err := cmdLink(dotfiles, false, false)
	if err != nil {
		t.Fatal(err)
	}
	// No error, no crash -- it should silently skip
}

func TestLinkWritesLockfile(t *testing.T) {
	dotfiles, home := setupTestDotfiles(t)
	stateDir := t.TempDir()
	t.Setenv("PUNCH_STATE_DIR", stateDir)

	writeFile(t, filepath.Join(dotfiles, "mod", "config"), "content\n")
	writeDotYaml(t, filepath.Join(dotfiles, "mod"), `
files:
  config: ~/.config/app/config
`)

	err := cmdLink(dotfiles, false, false)
	if err != nil {
		t.Fatal(err)
	}

	lockPath := filepath.Join(stateDir, "lock.json")
	data, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}

	var lf Lockfile
	if err := json.Unmarshal(data, &lf); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(home, ".config", "app", "config")
	entry, ok := lf.Files[target]
	if !ok {
		t.Fatalf("lockfile missing entry for %s", target)
	}
	if entry.Module != "mod" {
		t.Errorf("expected module 'mod', got %q", entry.Module)
	}
	if entry.SourceHash == "" {
		t.Error("source_hash is empty")
	}
	if entry.TargetHash == "" {
		t.Error("target_hash is empty")
	}
	if entry.InstalledAt == "" {
		t.Error("installed_at is empty")
	}
}

func TestLinkCopiesDirectories(t *testing.T) {
	dotfiles, home := setupTestDotfiles(t)
	t.Setenv("PUNCH_STATE_DIR", t.TempDir())

	// Create a skill directory with multiple files
	skillDir := filepath.Join(dotfiles, "ai", "skills", "my-skill")
	writeFile(t, filepath.Join(skillDir, "SKILL.md"), "# My Skill\n")
	writeFile(t, filepath.Join(skillDir, "references", "guide.md"), "guide content\n")
	writeDotYaml(t, filepath.Join(dotfiles, "ai"), `
files:
  skills/my-skill: ~/.copilot/skills/my-skill
`)

	err := cmdLink(dotfiles, false, false)
	if err != nil {
		t.Fatal(err)
	}

	// Check files were copied
	target := filepath.Join(home, ".copilot", "skills", "my-skill")
	got := readFile(t, filepath.Join(target, "SKILL.md"))
	if got != "# My Skill\n" {
		t.Errorf("unexpected SKILL.md content: %q", got)
	}
	got = readFile(t, filepath.Join(target, "references", "guide.md"))
	if got != "guide content\n" {
		t.Errorf("unexpected guide.md content: %q", got)
	}

	// Verify not a symlink
	info, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Error("target dir is a symlink, should be a copy")
	}
}

func TestDryRunDoesNotMutate(t *testing.T) {
	dotfiles, home := setupTestDotfiles(t)
	t.Setenv("PUNCH_STATE_DIR", t.TempDir())

	writeFile(t, filepath.Join(dotfiles, "mod", "file"), "content\n")
	writeDotYaml(t, filepath.Join(dotfiles, "mod"), `
files:
  file: ~/.file
`)

	err := cmdLink(dotfiles, false, true) // dry-run
	if err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(home, ".file")
	if _, err := os.Stat(target); err == nil {
		t.Error("dry-run should not have created the file")
	}
}

func TestExtractInstallCmd(t *testing.T) {
	tests := []struct {
		name   string
		input  any
		expect string
	}{
		{"string", "brew install git", "brew install git"},
		{"false string", "false", ""},
		{"bool false", false, ""},
		{"nil", nil, ""},
		{"map with cmd", map[string]any{"cmd": "bash -c 'echo hi'"}, "bash -c 'echo hi'"},
		{"map without cmd", map[string]any{"depends": []string{"foo"}}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractInstallCmd(tt.input)
			if got != tt.expect {
				t.Errorf("expected %q, got %q", tt.expect, got)
			}
		})
	}
}

// Helper to unmarshal yaml since the import name in tests needs to match
func yamlUnmarshal(data []byte, v any) error {
	return yaml.Unmarshal(data, v)
}
