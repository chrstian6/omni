package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

var errAgentIncomplete = errors.New("an agent needs at least a name and a description")

// Agents deployment surface. Claude Code subagents are single Markdown files in
// ~/.claude/agents/<name>.md: YAML frontmatter (name, description, tools, model)
// followed by the system prompt. The dashboard can list them, install ones from
// ~/Downloads, and author a new one from a small form.

type Agent struct {
	Name        string
	Description string
	Tools       string
	Model       string
	Path        string
	Installed   bool
}

func agentsDir() string {
	return filepath.Join(homeDir(), ".claude", "agents")
}

func InstalledAgents() []Agent {
	return agentsFrom(agentsDir(), true)
}

// ProjectAgentNames lists agents defined inside a project (its own
// .claude/agents), available to a session running there.
func ProjectAgentNames(projectDir string) []string {
	if projectDir == "" {
		return nil
	}
	var out []string
	for _, a := range agentsFrom(filepath.Join(projectDir, ".claude", "agents"), true) {
		out = append(out, a.Name)
	}
	return out
}

// DownloadsAgents finds .md files in ~/Downloads whose frontmatter names an
// agent (has both name and description) — the same shape Claude writes.
func DownloadsAgents() []Agent {
	var out []Agent
	entries, _ := os.ReadDir(downloadsDir())
	installed := map[string]bool{}
	for _, a := range InstalledAgents() {
		installed[a.Name] = true
	}
	for _, e := range entries {
		if e.IsDir() || strings.ToLower(filepath.Ext(e.Name())) != ".md" {
			continue
		}
		path := filepath.Join(downloadsDir(), e.Name())
		a, ok := parseAgentFile(path)
		if !ok {
			continue
		}
		a.Installed = installed[a.Name]
		out = append(out, a)
	}
	return out
}

func agentsFrom(dir string, installed bool) []Agent {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Agent
	for _, e := range entries {
		if e.IsDir() || strings.ToLower(filepath.Ext(e.Name())) != ".md" {
			continue
		}
		a, ok := parseAgentFile(filepath.Join(dir, e.Name()))
		if !ok {
			continue
		}
		a.Installed = installed
		out = append(out, a)
	}
	return out
}

func parseAgentFile(path string) (Agent, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Agent{}, false
	}
	text := string(data)
	if !strings.HasPrefix(strings.TrimSpace(text), "---") {
		return Agent{}, false
	}
	parts := strings.SplitN(text, "---", 3)
	if len(parts) < 3 {
		return Agent{}, false
	}
	a := Agent{Path: path}
	for _, line := range strings.Split(parts[1], "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(strings.Trim(strings.TrimSpace(val), `"'`))
		switch key {
		case "name":
			a.Name = val
		case "description":
			a.Description = val
		case "tools":
			a.Tools = val
		case "model":
			a.Model = val
		}
	}
	// An agent needs at least a name and a description to be usable.
	if a.Name == "" || a.Description == "" {
		return Agent{}, false
	}
	return a, true
}

func InstallAgent(a Agent) error {
	if err := os.MkdirAll(agentsDir(), 0o755); err != nil {
		return err
	}
	dest := filepath.Join(agentsDir(), safeName(a.Name)+".md")
	data, err := os.ReadFile(a.Path)
	if err != nil {
		return err
	}
	return os.WriteFile(dest, data, 0o644)
}

func RemoveAgent(a Agent) error {
	dest := filepath.Join(agentsDir(), safeName(a.Name)+".md")
	if a.Installed && strings.HasPrefix(a.Path, agentsDir()) {
		dest = a.Path
	}
	return os.Remove(dest)
}

// CreateAgent authors a new agent file from the form fields. Tools defaults to
// all ("*") when left blank, matching Claude's own default.
func CreateAgent(name, description, tools, model, prompt string) error {
	if strings.TrimSpace(name) == "" || strings.TrimSpace(description) == "" {
		return errAgentIncomplete
	}
	if err := os.MkdirAll(agentsDir(), 0o755); err != nil {
		return err
	}
	if strings.TrimSpace(tools) == "" {
		tools = "*"
	}

	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("name: " + name + "\n")
	b.WriteString("description: " + yamlScalar(description) + "\n")
	b.WriteString("tools: " + tools + "\n")
	if strings.TrimSpace(model) != "" {
		b.WriteString("model: " + model + "\n")
	}
	b.WriteString("---\n\n")
	if strings.TrimSpace(prompt) == "" {
		prompt = "You are a helpful assistant."
	}
	b.WriteString(prompt)
	b.WriteString("\n")

	dest := filepath.Join(agentsDir(), safeName(name)+".md")
	return os.WriteFile(dest, []byte(b.String()), 0o644)
}

// yamlScalar wraps a description in quotes when it contains characters that
// would otherwise break a bare YAML scalar (a colon, leading punctuation).
func yamlScalar(s string) string {
	if strings.ContainsAny(s, ":#\n") || strings.HasPrefix(s, "-") {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return s
}
