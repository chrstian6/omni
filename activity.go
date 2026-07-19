package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type TodoItem struct {
	ID         string
	Subject    string
	ActiveForm string
	Status     string // pending | in_progress | completed
}

// Activity is what a session is doing, assembled from the two places Claude
// leaves traces: the task files, and the transcript's own summary records.
type Activity struct {
	Title      string
	LastPrompt string
	Todos      []TodoItem
	Steps      []string   // ordered recap of the session's prompts, oldest→newest
	Agents     []Subagent // background/Task agents this session has launched
}

func (a Activity) Current() (TodoItem, bool) {
	for _, t := range a.Todos {
		if t.Status == "in_progress" {
			return t, true
		}
	}
	return TodoItem{}, false
}

func (a Activity) DoneCount() int {
	n := 0
	for _, t := range a.Todos {
		if t.Status == "completed" {
			n++
		}
	}
	return n
}

func homeDir() string {
	home, _ := os.UserHomeDir()
	return home
}

// LoadActivity does blocking file reads — call it off the UI goroutine.
func LoadActivity(sessionID string) Activity {
	a := Activity{Todos: loadTodos(sessionID)}
	if transcript := findTranscript(sessionID); transcript != "" {
		a.Title, a.LastPrompt = loadSummary(transcript)
	}
	a.Steps = loadSteps(sessionID)
	a.Agents = loadSubagents(sessionID)
	return a
}

func loadTodos(sessionID string) []TodoItem {
	dir := filepath.Join(homeDir(), ".claude", "tasks", sessionID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var items []TodoItem
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal(data, &obj); err != nil {
			continue
		}
		id, _ := obj["id"].(string)
		if id == "" {
			continue
		}
		items = append(items, TodoItem{
			ID:         id,
			Subject:    str(obj["subject"]),
			ActiveForm: str(obj["activeForm"]),
			Status:     orDefault(str(obj["status"]), "pending"),
		})
	}

	// Files are named 1.json, 2.json… so numeric order is authoring order.
	sort.Slice(items, func(i, j int) bool {
		ni, _ := strconv.Atoi(items[i].ID)
		nj, _ := strconv.Atoi(items[j].ID)
		return ni < nj
	})
	return items
}

// findTranscript searches by filename since reproducing Claude's cwd-encoding
// rules for the project directory isn't worth the coupling.
func findTranscript(sessionID string) string {
	projects := filepath.Join(homeDir(), ".claude", "projects")
	dirs, err := os.ReadDir(projects)
	if err != nil {
		return ""
	}
	for _, d := range dirs {
		candidate := filepath.Join(projects, d.Name(), sessionID+".jsonl")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

// loadSummary scans the transcript backwards so the most recent title/prompt
// records win, stopping once both are found.
func loadSummary(path string) (title, lastPrompt string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			continue
		}
		switch obj["type"] {
		case "ai-title":
			if title == "" {
				title = str(obj["aiTitle"])
			}
		case "last-prompt":
			if lastPrompt == "" {
				lastPrompt = str(obj["lastPrompt"])
			}
		}
		if title != "" && lastPrompt != "" {
			break
		}
	}
	return title, lastPrompt
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
