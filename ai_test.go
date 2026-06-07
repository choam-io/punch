package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestClassifyMCPServer(t *testing.T) {
	tests := []struct {
		name       string
		srvName    string
		raw        string
		expectType string
		expectPkg  string
		expectVer  string
		expectURL  string
	}{
		{
			name:       "npm with @latest",
			srvName:    "playwright",
			raw:        `{"type":"local","command":"npx","args":["@playwright/mcp@latest","--extension"]}`,
			expectType: "npm",
			expectPkg:  "@playwright/mcp",
			expectVer:  "latest",
		},
		{
			name:       "npm with exact version",
			srvName:    "playwright",
			raw:        `{"type":"local","command":"npx","args":["@playwright/mcp@0.0.42","--extension"]}`,
			expectType: "npm",
			expectPkg:  "@playwright/mcp",
			expectVer:  "0.0.42",
		},
		{
			name:       "npm with -y flag before package",
			srvName:    "azure-mcp",
			raw:        `{"command":"npx","args":["-y","@azure/mcp@latest","server","start"]}`,
			expectType: "npm",
			expectPkg:  "@azure/mcp",
			expectVer:  "latest",
		},
		{
			name:       "http server",
			srvName:    "datadog",
			raw:        `{"type":"http","url":"https://mcp.datadoghq.com/api/mcp"}`,
			expectType: "http",
			expectURL:  "https://mcp.datadoghq.com/api/mcp",
		},
		{
			name:       "sse server",
			srvName:    "remote",
			raw:        `{"type":"sse","url":"https://example.com/sse"}`,
			expectType: "http",
			expectURL:  "https://example.com/sse",
		},
		{
			name:       "local command",
			srvName:    "custom",
			raw:        `{"type":"local","command":"cs-mcp-bridge","args":["--port","8080"]}`,
			expectType: "local",
		},
		{
			name:       "local uv command",
			srvName:    "fabric",
			raw:        `{"type":"local","command":"uv","args":["--directory","/path","run","python","script"]}`,
			expectType: "local",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srvType, pkg, version, url, _ := classifyMCPServer(tt.srvName, json.RawMessage(tt.raw))
			if srvType != tt.expectType {
				t.Errorf("type: expected %q, got %q", tt.expectType, srvType)
			}
			if tt.expectPkg != "" && pkg != tt.expectPkg {
				t.Errorf("package: expected %q, got %q", tt.expectPkg, pkg)
			}
			if tt.expectVer != "" && version != tt.expectVer {
				t.Errorf("version: expected %q, got %q", tt.expectVer, version)
			}
			if tt.expectURL != "" && url != tt.expectURL {
				t.Errorf("url: expected %q, got %q", tt.expectURL, url)
			}
		})
	}
}

func TestHashDirDetailed(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "hello")
	writeFile(t, filepath.Join(dir, "sub", "b.txt"), "world")

	hash1, count, err := hashDirDetailed(dir)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("expected 2 files, got %d", count)
	}
	if hash1 == "" {
		t.Fatal("hash is empty")
	}

	// Same content → same hash
	dir2 := t.TempDir()
	writeFile(t, filepath.Join(dir2, "a.txt"), "hello")
	writeFile(t, filepath.Join(dir2, "sub", "b.txt"), "world")
	hash2, _, _ := hashDirDetailed(dir2)
	if hash1 != hash2 {
		t.Error("identical dirs should produce identical hashes")
	}

	// Different content → different hash
	dir3 := t.TempDir()
	writeFile(t, filepath.Join(dir3, "a.txt"), "hello")
	writeFile(t, filepath.Join(dir3, "sub", "b.txt"), "changed")
	hash3, _, _ := hashDirDetailed(dir3)
	if hash1 == hash3 {
		t.Error("different content should produce different hashes")
	}
}

func TestHashJSON(t *testing.T) {
	// Same logical JSON → same hash regardless of formatting
	a := json.RawMessage(`{"b":2,"a":1}`)
	b := json.RawMessage(`{  "b": 2,  "a": 1 }`)
	if hashJSON(a) != hashJSON(b) {
		t.Error("equivalent JSON should produce identical hashes")
	}

	// Different JSON → different hash
	c := json.RawMessage(`{"a":1,"b":3}`)
	if hashJSON(a) == hashJSON(c) {
		t.Error("different JSON should produce different hashes")
	}
}

func TestFindSkillDirs(t *testing.T) {
	dotfiles := t.TempDir()
	writeFile(t, filepath.Join(dotfiles, "ai", "skills", "code-review", "SKILL.md"), "# Code Review\n")
	writeFile(t, filepath.Join(dotfiles, "ai", "skills", "git", "SKILL.md"), "# Git\n")
	writeFile(t, filepath.Join(dotfiles, "ai", "skills", "git", "references", "guide.md"), "guide\n")
	writeFile(t, filepath.Join(dotfiles, "plugins", "org", "repo", "skills", "kusto", "SKILL.md"), "# Kusto\n")

	skills := findSkillDirs(dotfiles)
	if len(skills) != 3 {
		t.Fatalf("expected 3 skills, got %d: %v", len(skills), skills)
	}
	if skills["code-review"] != filepath.Join("ai", "skills", "code-review") {
		t.Errorf("code-review path: %q", skills["code-review"])
	}
	if skills["git"] != filepath.Join("ai", "skills", "git") {
		t.Errorf("git path: %q", skills["git"])
	}
	if skills["kusto"] != filepath.Join("plugins", "org", "repo", "skills", "kusto") {
		t.Errorf("kusto path: %q", skills["kusto"])
	}
}

func TestFindMCPConfigs(t *testing.T) {
	dotfiles := t.TempDir()
	t.Setenv("HOME", t.TempDir()) // isolate from real ~/.copilot

	writeFile(t, filepath.Join(dotfiles, "mcp-config.json"), `{"mcpServers":{}}`)
	writeFile(t, filepath.Join(dotfiles, "mcp-config.work.json"), `{"mcpServers":{}}`)
	writeFile(t, filepath.Join(dotfiles, "plugins", "org", "repo", "ai", "mcp-config.personal.json"), `{"mcpServers":{}}`)

	configs := findMCPConfigs(dotfiles)
	if len(configs) < 3 {
		t.Errorf("expected at least 3 configs, got %d: %v", len(configs), configs)
	}
}

func TestAILockAndVerify(t *testing.T) {
	dotfiles := t.TempDir()
	t.Setenv("HOME", t.TempDir())

	// Create a skill
	writeFile(t, filepath.Join(dotfiles, "skills", "test-skill", "SKILL.md"), "# Test Skill\n")

	// Create MCP config with an http server (no npm resolution needed)
	mcpConfig := map[string]any{
		"mcpServers": map[string]any{
			"test-http": map[string]any{
				"type": "http",
				"url":  "https://example.com/mcp",
			},
			"test-local": map[string]any{
				"type":    "local",
				"command": "my-server",
				"args":    []string{"--port", "8080"},
			},
		},
	}
	data, _ := json.MarshalIndent(mcpConfig, "", "  ")
	writeFile(t, filepath.Join(dotfiles, "mcp-config.json"), string(data))

	// Lock
	err := cmdAILock(dotfiles)
	if err != nil {
		t.Fatal(err)
	}

	// Check lockfile was created
	lockPath := filepath.Join(dotfiles, "ai.lock.yaml")
	lockData, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}

	var lf AILockfile
	if err := yaml.Unmarshal(lockData, &lf); err != nil {
		t.Fatal(err)
	}

	if lf.Version != 1 {
		t.Errorf("version: expected 1, got %d", lf.Version)
	}
	if lf.LockedAt == "" {
		t.Error("locked_at is empty")
	}
	if len(lf.Skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(lf.Skills))
	}
	skill, ok := lf.Skills["test-skill"]
	if !ok {
		t.Fatal("missing test-skill")
	}
	if skill.FileCount != 1 {
		t.Errorf("file_count: expected 1, got %d", skill.FileCount)
	}
	if skill.SHA256 == "" {
		t.Error("skill sha256 is empty")
	}

	if len(lf.MCPServers) != 2 {
		t.Fatalf("expected 2 servers, got %d", len(lf.MCPServers))
	}
	httpSrv := lf.MCPServers["test-http"]
	if httpSrv.Type != "http" {
		t.Errorf("http type: %q", httpSrv.Type)
	}
	if httpSrv.URL != "https://example.com/mcp" {
		t.Errorf("http url: %q", httpSrv.URL)
	}

	// Verify should pass
	err = cmdAIVerify(dotfiles)
	if err != nil {
		t.Fatalf("verify should pass: %v", err)
	}

	// Modify skill → verify should fail
	writeFile(t, filepath.Join(dotfiles, "skills", "test-skill", "SKILL.md"), "# Modified\n")
	err = cmdAIVerify(dotfiles)
	if err == nil {
		t.Fatal("verify should fail after modification")
	}
}

func TestAIList(t *testing.T) {
	dotfiles := t.TempDir()
	t.Setenv("HOME", t.TempDir())

	writeFile(t, filepath.Join(dotfiles, "skills", "locked-skill", "SKILL.md"), "# Locked\n")
	writeFile(t, filepath.Join(dotfiles, "skills", "unlocked-skill", "SKILL.md"), "# Unlocked\n")

	mcpConfig := map[string]any{
		"mcpServers": map[string]any{
			"test-http": map[string]any{
				"type": "http",
				"url":  "https://example.com/mcp",
			},
		},
	}
	data, _ := json.MarshalIndent(mcpConfig, "", "  ")
	writeFile(t, filepath.Join(dotfiles, "mcp-config.json"), string(data))

	// Lock only partially (lock first, then add unlocked skill)
	err := cmdAILock(dotfiles)
	if err != nil {
		t.Fatal(err)
	}

	// List should show both locked and unlocked
	err = cmdAIList(dotfiles)
	if err != nil {
		t.Fatal(err)
	}
}

func TestAIPin(t *testing.T) {
	dotfiles := t.TempDir()
	t.Setenv("HOME", t.TempDir())

	// Config with no @latest — pin should be a no-op
	mcpConfig := map[string]any{
		"mcpServers": map[string]any{
			"test-http": map[string]any{
				"type": "http",
				"url":  "https://example.com/mcp",
			},
		},
	}
	data, _ := json.MarshalIndent(mcpConfig, "", "  ")
	writeFile(t, filepath.Join(dotfiles, "mcp-config.json"), string(data))

	// Pin with no npm servers should be a no-op
	err := cmdAIPin(dotfiles, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Verify config unchanged
	got := readFile(t, filepath.Join(dotfiles, "mcp-config.json"))
	if got != string(data) {
		t.Error("config should not have changed")
	}
}

func TestMergeKeys(t *testing.T) {
	a := map[string]string{"b": "1", "a": "2"}
	b := map[string]int{"c": 3, "a": 4}
	keys := mergeKeys(a, b)
	if len(keys) != 3 {
		t.Fatalf("expected 3 keys, got %d", len(keys))
	}
	if keys[0] != "a" || keys[1] != "b" || keys[2] != "c" {
		t.Errorf("expected [a b c], got %v", keys)
	}
}
