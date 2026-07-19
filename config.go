package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
)

// Nicknames are held locally (like the Swift app's UserDefaults) because the
// Claude process rewrites its own registry file and can overwrite the name
// field there — a local copy survives that.
type Config struct {
	Nicknames map[string]string `json:"nicknames"`
}

func configPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".omni", "config.json")
}

func LoadConfig() Config {
	data, err := os.ReadFile(configPath())
	if err != nil {
		return Config{Nicknames: map[string]string{}}
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil || c.Nicknames == nil {
		c.Nicknames = map[string]string{}
	}
	return c
}

func (c Config) Save() {
	path := configPath()
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o644)
}

func (c Config) DisplayName(s Session) string {
	if n, ok := c.Nicknames[s.SessionID]; ok && n != "" {
		return n
	}
	return s.Name
}

// writeRegistryName best-effort mirrors the rename into the session's own
// registry file so other Claude surfaces (the Mac HUD, another terminal) see
// it too. Rewritten as a generic map to preserve fields this program doesn't
// know about.
func writeRegistryName(s Session, name string) {
	file := filepath.Join(sessionsDir(), strconv.Itoa(int(s.PID))+".json")
	data, err := os.ReadFile(file)
	if err != nil {
		return
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return
	}
	obj["name"] = name
	obj["nameSource"] = "user"
	out, err := json.Marshal(obj)
	if err != nil {
		return
	}
	_ = os.WriteFile(file, out, 0o644)
}
