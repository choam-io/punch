package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ── ai.lock.yaml schema ──────────────────────────────────────────

type AILockfile struct {
	Version    int                       `yaml:"version"`
	LockedAt   string                    `yaml:"locked_at"`
	Skills     map[string]*LockedSkill   `yaml:"skills,omitempty"`
	MCPServers map[string]*LockedMCP     `yaml:"mcp_servers,omitempty"`
}

type LockedSkill struct {
	Path      string `yaml:"path"`
	SHA256    string `yaml:"sha256"`
	FileCount int    `yaml:"file_count"`
}

type LockedMCP struct {
	Type         string `yaml:"type"`                    // npm, http, local
	Package      string `yaml:"package,omitempty"`        // npm package name
	Version      string `yaml:"version,omitempty"`        // resolved version
	URL          string `yaml:"url,omitempty"`            // http URL
	Script       string `yaml:"script,omitempty"`         // local script path
	ScriptSHA256 string `yaml:"script_sha256,omitempty"`
	ConfigSHA256 string `yaml:"config_sha256"`
}

// ── MCP config parsing ───────────────────────────────────────────

type MCPConfig struct {
	Servers map[string]json.RawMessage `json:"mcpServers"`
}

type MCPServerEntry struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
	URL     string   `json:"url"`
}

// npmPkgRe matches @scope/pkg@version or pkg@version patterns in npx args.
var npmPkgRe = regexp.MustCompile(`^(@[a-zA-Z0-9._-]+/[a-zA-Z0-9._-]+|[a-zA-Z0-9._-]+)@(.+)$`)

// npmPkgNoVersionRe matches @scope/pkg (no version) patterns.
var npmPkgNoVersionRe = regexp.MustCompile(`^@[a-zA-Z0-9._-]+/[a-zA-Z0-9._-]+$`)

// classifyMCPServer determines the type and extracts metadata from a raw config entry.
func classifyMCPServer(name string, raw json.RawMessage) (serverType, pkg, version, url, script string) {
	var entry MCPServerEntry
	_ = json.Unmarshal(raw, &entry)

	if entry.URL != "" && (entry.Type == "http" || entry.Type == "sse") {
		return "http", "", "", entry.URL, ""
	}

	if entry.Command == "npx" {
		// Find the npm package arg (skip flags like -y)
		for _, arg := range entry.Args {
			if strings.HasPrefix(arg, "-") {
				continue
			}
			if m := npmPkgRe.FindStringSubmatch(arg); m != nil {
				return "npm", m[1], m[2], "", ""
			}
			if npmPkgNoVersionRe.MatchString(arg) {
				return "npm", arg, "latest", "", ""
			}
		}
	}

	// Local: anything else with a command
	if entry.Command != "" {
		return "local", "", "", "", entry.Command
	}

	return "unknown", "", "", "", ""
}

// ── Discovery ────────────────────────────────────────────────────

// findMCPConfigs locates mcp-config*.json files in dotfiles and ~/.copilot/.
func findMCPConfigs(dotfilesDir string) []string {
	seen := make(map[string]bool) // resolved real paths for dedup
	var configs []string

	addIfNew := func(path string) {
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			real = path
		}
		abs, err := filepath.Abs(real)
		if err != nil {
			abs = real
		}
		if seen[abs] {
			return
		}
		seen[abs] = true
		configs = append(configs, path)
	}

	// Check dotfiles root
	primary := filepath.Join(dotfilesDir, "mcp-config.json")
	if _, err := os.Stat(primary); err == nil {
		addIfNew(primary)
	}

	// Glob for variants (mcp-config.work.json, mcp-config.personal.json, etc.)
	matches, _ := filepath.Glob(filepath.Join(dotfilesDir, "mcp-config.*.json"))
	for _, m := range matches {
		addIfNew(m)
	}

	// Recursively search in subdirectories (plugins, ai/, etc.)
	_ = filepath.WalkDir(dotfilesDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			base := d.Name()
			if base == ".git" || base == "node_modules" || base == ".cache" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), "mcp-config") && strings.HasSuffix(d.Name(), ".json") {
			addIfNew(path)
		}
		return nil
	})

	// Check ~/.copilot/ (may be a symlink into dotfilesDir — dedup handles it)
	home, _ := os.UserHomeDir()
	if home != "" {
		copilotConfig := filepath.Join(home, ".copilot", "mcp-config.json")
		if _, err := os.Stat(copilotConfig); err == nil {
			addIfNew(copilotConfig)
		}
	}

	return configs
}

// findSkillDirs locates directories containing SKILL.md files.
func findSkillDirs(dotfilesDir string) map[string]string {
	skills := make(map[string]string) // name -> relative path

	_ = filepath.WalkDir(dotfilesDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			base := d.Name()
			if base == ".git" || base == "node_modules" || base == ".cache" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == "SKILL.md" {
			skillDir := filepath.Dir(path)
			rel, _ := filepath.Rel(dotfilesDir, skillDir)
			name := filepath.Base(skillDir)
			skills[name] = rel
		}
		return nil
	})

	return skills
}

// ── Hashing ──────────────────────────────────────────────────────

// hashDirDetailed computes a deterministic sha256 over all files in a directory,
// returning the hash, file count, and any error. Files are sorted by relative
// path; each file's path and content are fed into a running hash.
func hashDirDetailed(dir string) (string, int, error) {
	h := sha256.New()
	var count int

	var paths []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		paths = append(paths, rel)
		return nil
	})
	if err != nil {
		return "", 0, err
	}

	sort.Strings(paths)
	for _, rel := range paths {
		data, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			return "", 0, err
		}
		// Include the path as a separator to distinguish files
		h.Write([]byte(rel))
		h.Write([]byte{0})
		h.Write(data)
		count++
	}

	return hex.EncodeToString(h.Sum(nil)), count, nil
}

// hashJSON computes sha256 of a raw JSON block (compact-encoded for determinism).
func hashJSON(raw json.RawMessage) string {
	// Re-marshal to get deterministic compact JSON
	var v any
	if json.Unmarshal(raw, &v) != nil {
		h := sha256.Sum256(raw)
		return hex.EncodeToString(h[:])
	}
	compact, err := json.Marshal(v)
	if err != nil {
		h := sha256.Sum256(raw)
		return hex.EncodeToString(h[:])
	}
	h := sha256.Sum256(compact)
	return hex.EncodeToString(h[:])
}

// ── npm version resolution ───────────────────────────────────────

// npmResolveVersion runs `npm view <pkg> version` and returns the result.
func npmResolveVersion(pkg string) (string, error) {
	cmd := exec.Command("npm", "view", pkg, "version")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("npm view %s version: %w", pkg, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// ── AI lockfile I/O ──────────────────────────────────────────────

func loadAILockfile(dotfilesDir string) *AILockfile {
	lf := &AILockfile{
		Version:    1,
		Skills:     make(map[string]*LockedSkill),
		MCPServers: make(map[string]*LockedMCP),
	}
	data, err := os.ReadFile(filepath.Join(dotfilesDir, "ai.lock.yaml"))
	if err != nil {
		return lf
	}
	_ = yaml.Unmarshal(data, lf)
	if lf.Skills == nil {
		lf.Skills = make(map[string]*LockedSkill)
	}
	if lf.MCPServers == nil {
		lf.MCPServers = make(map[string]*LockedMCP)
	}
	return lf
}

func saveAILockfile(dotfilesDir string, lf *AILockfile) error {
	lf.LockedAt = time.Now().UTC().Format(time.RFC3339)
	data, err := yaml.Marshal(lf)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dotfilesDir, "ai.lock.yaml"), data, 0o644)
}

// ── Commands ─────────────────────────────────────────────────────

func cmdAILock(dotfilesDir string) error {
	lf := &AILockfile{
		Version:    1,
		Skills:     make(map[string]*LockedSkill),
		MCPServers: make(map[string]*LockedMCP),
	}

	// Lock skills
	skills := findSkillDirs(dotfilesDir)
	names := sortedKeys(skills)
	for _, name := range names {
		relPath := skills[name]
		absPath := filepath.Join(dotfilesDir, relPath)
		hash, count, err := hashDirDetailed(absPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ⚠ %s: %v\n", name, err)
			continue
		}
		lf.Skills[name] = &LockedSkill{
			Path:      relPath,
			SHA256:    hash,
			FileCount: count,
		}
		fmt.Printf("  🔒 skill %-24s %d files  sha256:%s\n", name, count, hash[:16])
	}

	// Lock MCP servers
	configs := findMCPConfigs(dotfilesDir)
	seen := make(map[string]bool)
	for _, cfgPath := range configs {
		data, err := os.ReadFile(cfgPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ⚠ %s: %v\n", cfgPath, err)
			continue
		}
		var cfg MCPConfig
		if err := json.Unmarshal(data, &cfg); err != nil {
			fmt.Fprintf(os.Stderr, "  ⚠ %s: %v\n", cfgPath, err)
			continue
		}
		rel, _ := filepath.Rel(dotfilesDir, cfgPath)
		fmt.Printf("\n\033[1mScanning \033[38;5;12m%s\033[0m\n", rel)

		serverNames := sortedKeysRaw(cfg.Servers)
		for _, srvName := range serverNames {
			if seen[srvName] {
				continue
			}
			seen[srvName] = true
			raw := cfg.Servers[srvName]
			srvType, pkg, version, url, script := classifyMCPServer(srvName, raw)
			configHash := hashJSON(raw)

			locked := &LockedMCP{
				Type:         srvType,
				ConfigSHA256: configHash,
			}

			switch srvType {
			case "npm":
				locked.Package = pkg
				if version == "latest" {
					resolved, err := npmResolveVersion(pkg)
					if err != nil {
						fmt.Fprintf(os.Stderr, "  ⚠ %s: %v (keeping @latest)\n", srvName, err)
						locked.Version = "latest"
					} else {
						locked.Version = resolved
						fmt.Printf("  🔒 mcp %-24s npm %s@%s (resolved from @latest)\n", srvName, pkg, resolved)
					}
				} else {
					locked.Version = version
					fmt.Printf("  🔒 mcp %-24s npm %s@%s\n", srvName, pkg, version)
				}
			case "http":
				locked.URL = url
				fmt.Printf("  🔒 mcp %-24s http %s\n", srvName, truncateURL(url))
			case "local":
				locked.Script = script
				// Try to hash the script/command if it's a file path
				if scriptHash := hashFile(script); scriptHash != "" {
					locked.ScriptSHA256 = scriptHash
				}
				fmt.Printf("  🔒 mcp %-24s local %s\n", srvName, script)
			default:
				fmt.Printf("  🔒 mcp %-24s %s\n", srvName, srvType)
			}

			lf.MCPServers[srvName] = locked
		}
	}

	if err := saveAILockfile(dotfilesDir, lf); err != nil {
		return fmt.Errorf("saving ai.lock.yaml: %w", err)
	}
	fmt.Printf("\n✅ %d skill(s), %d server(s) locked to ai.lock.yaml\n", len(lf.Skills), len(lf.MCPServers))
	return nil
}

func cmdAIVerify(dotfilesDir string) error {
	lf := loadAILockfile(dotfilesDir)
	if len(lf.Skills) == 0 && len(lf.MCPServers) == 0 {
		return fmt.Errorf("no ai.lock.yaml found (run 'punch ai lock' first)")
	}

	ok := 0
	problems := 0

	// Verify skills
	names := sortedKeys(lf.Skills)
	for _, name := range names {
		locked := lf.Skills[name]
		absPath := filepath.Join(dotfilesDir, locked.Path)
		hash, count, err := hashDirDetailed(absPath)
		if err != nil {
			fmt.Printf("  ❌ skill %s: %v\n", name, err)
			problems++
			continue
		}
		if hash != locked.SHA256 {
			fmt.Printf("  ❌ skill %s: content changed (files: %d→%d)\n", name, locked.FileCount, count)
			problems++
		} else {
			fmt.Printf("  ✅ skill %s: %d files, hash matches\n", name, count)
			ok++
		}
	}

	// Verify MCP servers
	configs := findMCPConfigs(dotfilesDir)
	currentServers := make(map[string]json.RawMessage)
	for _, cfgPath := range configs {
		data, err := os.ReadFile(cfgPath)
		if err != nil {
			continue
		}
		var cfg MCPConfig
		if json.Unmarshal(data, &cfg) != nil {
			continue
		}
		for k, v := range cfg.Servers {
			if _, exists := currentServers[k]; !exists {
				currentServers[k] = v
			}
		}
	}

	srvNames := sortedKeys(lf.MCPServers)
	for _, name := range srvNames {
		locked := lf.MCPServers[name]
		raw, found := currentServers[name]
		if !found {
			fmt.Printf("  ❌ mcp %s: server removed from config\n", name)
			problems++
			continue
		}

		configHash := hashJSON(raw)
		if configHash != locked.ConfigSHA256 {
			fmt.Printf("  ❌ mcp %s: config changed\n", name)
			problems++
			continue
		}

		// For npm servers, check if version drifted
		if locked.Type == "npm" && locked.Version != "latest" {
			_, _, version, _, _ := classifyMCPServer(name, raw)
			if version == "latest" {
				resolved, err := npmResolveVersion(locked.Package)
				if err == nil && resolved != locked.Version {
					fmt.Printf("  ⚠  mcp %s: npm %s@%s available (locked: %s)\n", name, locked.Package, resolved, locked.Version)
				}
			}
		}

		// For local servers with hashed scripts, verify
		if locked.ScriptSHA256 != "" {
			current := hashFile(locked.Script)
			if current != "" && current != locked.ScriptSHA256 {
				fmt.Printf("  ❌ mcp %s: script changed\n", name)
				problems++
				continue
			}
		}

		fmt.Printf("  ✅ mcp %s: %s\n", name, mcpSummary(locked))
		ok++
	}

	fmt.Printf("\n%d ok, %d problems\n", ok, problems)
	if problems > 0 {
		return fmt.Errorf("%d item(s) have drift", problems)
	}
	return nil
}

func cmdAIList(dotfilesDir string) error {
	lf := loadAILockfile(dotfilesDir)

	// Discover current state
	skills := findSkillDirs(dotfilesDir)
	configs := findMCPConfigs(dotfilesDir)
	currentServers := make(map[string]json.RawMessage)
	for _, cfgPath := range configs {
		data, err := os.ReadFile(cfgPath)
		if err != nil {
			continue
		}
		var cfg MCPConfig
		if json.Unmarshal(data, &cfg) != nil {
			continue
		}
		for k, v := range cfg.Servers {
			if _, exists := currentServers[k]; !exists {
				currentServers[k] = v
			}
		}
	}

	// Skills
	fmt.Println("Skills:")
	allSkills := mergeKeys(skills, lf.Skills)
	sort.Strings(allSkills)
	if len(allSkills) == 0 {
		fmt.Println("  (none found)")
	}
	for _, name := range allSkills {
		relPath := skills[name]
		locked, isLocked := lf.Skills[name]
		if relPath == "" && locked != nil {
			relPath = locked.Path
		}
		if isLocked {
			fmt.Printf("  ✅ %-24s %-40s (%d files, locked)\n", name, relPath, locked.FileCount)
		} else {
			// Count files for unlocked skills
			absPath := filepath.Join(dotfilesDir, relPath)
			_, count, _ := hashDirDetailed(absPath)
			fmt.Printf("  ⚠  %-24s %-40s (%d files, not locked)\n", name, relPath, count)
		}
	}

	// MCP Servers
	fmt.Println("\nMCP Servers:")
	allServers := mergeKeys(currentServers, lf.MCPServers)
	sort.Strings(allServers)
	if len(allServers) == 0 {
		fmt.Println("  (none found)")
	}
	for _, name := range allServers {
		raw, inConfig := currentServers[name]
		locked, isLocked := lf.MCPServers[name]

		if isLocked && inConfig {
			fmt.Printf("  ✅ %-24s %s\n", name, mcpSummary(locked))
		} else if isLocked && !inConfig {
			fmt.Printf("  ❌ %-24s %s (removed from config)\n", name, mcpSummary(locked))
		} else if inConfig {
			srvType, pkg, version, url, script := classifyMCPServer(name, raw)
			desc := mcpDescribe(srvType, pkg, version, url, script)
			unpinned := ""
			if srvType == "npm" && version == "latest" {
				unpinned = " (unpinned)"
			}
			fmt.Printf("  ⚠  %-24s %s%s\n", name, desc, unpinned)
		}
	}

	return nil
}

func cmdAIPin(dotfilesDir string, servers []string) error {
	configs := findMCPConfigs(dotfilesDir)
	if len(configs) == 0 {
		return fmt.Errorf("no mcp-config*.json files found")
	}

	pinned := 0
	for _, cfgPath := range configs {
		data, err := os.ReadFile(cfgPath)
		if err != nil {
			continue
		}

		// Parse as generic map to preserve structure
		var doc map[string]any
		if err := json.Unmarshal(data, &doc); err != nil {
			continue
		}
		serversMap, ok := doc["mcpServers"].(map[string]any)
		if !ok {
			continue
		}

		modified := false
		for srvName, srvRaw := range serversMap {
			// If specific servers requested, filter
			if len(servers) > 0 && !contains(servers, srvName) {
				continue
			}

			srvMap, ok := srvRaw.(map[string]any)
			if !ok {
				continue
			}
			command, _ := srvMap["command"].(string)
			if command != "npx" {
				continue
			}
			args, ok := srvMap["args"].([]any)
			if !ok {
				continue
			}

			for i, argRaw := range args {
				arg, ok := argRaw.(string)
				if !ok || strings.HasPrefix(arg, "-") {
					continue
				}

				var pkg, version string
				if m := npmPkgRe.FindStringSubmatch(arg); m != nil {
					pkg, version = m[1], m[2]
				} else if npmPkgNoVersionRe.MatchString(arg) {
					pkg, version = arg, "latest"
				} else {
					continue
				}

				if version != "latest" {
					continue
				}

				resolved, err := npmResolveVersion(pkg)
				if err != nil {
					fmt.Fprintf(os.Stderr, "  ⚠ %s: %v\n", srvName, err)
					continue
				}

				newArg := pkg + "@" + resolved
				args[i] = newArg
				srvMap["args"] = args
				modified = true
				pinned++
				fmt.Printf("  📌 %s: %s → %s\n", srvName, arg, newArg)
			}
		}

		if modified {
			out, err := json.MarshalIndent(doc, "", "  ")
			if err != nil {
				return fmt.Errorf("marshaling %s: %w", cfgPath, err)
			}
			out = append(out, '\n')
			if err := os.WriteFile(cfgPath, out, 0o644); err != nil {
				return fmt.Errorf("writing %s: %w", cfgPath, err)
			}
			rel, _ := filepath.Rel(dotfilesDir, cfgPath)
			fmt.Printf("  💾 updated %s\n", rel)
		}
	}

	if pinned == 0 {
		fmt.Println("  (no @latest versions to pin)")
	} else {
		fmt.Printf("\n✅ %d server(s) pinned\n", pinned)
	}
	return nil
}

// ── Helpers ──────────────────────────────────────────────────────

func truncateURL(url string) string {
	// Show just the hostname for display
	url = strings.TrimPrefix(url, "https://")
	url = strings.TrimPrefix(url, "http://")
	if i := strings.Index(url, "/"); i > 0 {
		return url[:i]
	}
	return url
}

func mcpSummary(l *LockedMCP) string {
	switch l.Type {
	case "npm":
		return fmt.Sprintf("npm %s@%s", l.Package, l.Version)
	case "http":
		return fmt.Sprintf("http %s", truncateURL(l.URL))
	case "local":
		return fmt.Sprintf("local %s", l.Script)
	}
	return l.Type
}

func mcpDescribe(srvType, pkg, version, url, script string) string {
	switch srvType {
	case "npm":
		return fmt.Sprintf("npm %s@%s", pkg, version)
	case "http":
		return fmt.Sprintf("http %s", truncateURL(url))
	case "local":
		return fmt.Sprintf("local %s", script)
	}
	return srvType
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// sortedKeys returns sorted keys from a map[string]T.
func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sortedKeysRaw returns sorted keys from a map[string]json.RawMessage.
func sortedKeysRaw(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// mergeKeys returns sorted unique keys from two maps.
func mergeKeys[T any, U any](a map[string]T, b map[string]U) []string {
	seen := make(map[string]bool)
	for k := range a {
		seen[k] = true
	}
	for k := range b {
		seen[k] = true
	}
	return sortedKeys(seen)
}
