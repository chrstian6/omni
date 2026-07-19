package main

import (
	"archive/zip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var errSkillIncomplete = errors.New("a skill needs at least a name and a description")

// A skills inventory. Claude Code loads user skills from ~/.claude/skills/<name>/
// SKILL.md, each with YAML frontmatter naming and describing it. This gives the
// dashboard a browse-and-install surface: it lists what's installed and what's
// sitting in ~/Downloads waiting to be added (a folder with a SKILL.md, or a
// .zip containing one), and installs by copying or unzipping into place.

type Skill struct {
	Name        string
	Description string
	Path        string // source path (installed dir, downloads dir, or .zip)
	Installed   bool
	IsZip       bool
}

func skillsDir() string {
	return filepath.Join(homeDir(), ".claude", "skills")
}

func downloadsDir() string {
	return filepath.Join(homeDir(), "Downloads")
}

// ProjectSkillNames lists the skills defined inside a project (its own
// .claude/skills), which is what's actually available to a session running
// there beyond the global set.
func ProjectSkillNames(projectDir string) []string {
	if projectDir == "" {
		return nil
	}
	dir := filepath.Join(projectDir, ".claude", "skills")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), "SKILL.md")); err != nil {
			continue
		}
		name, _ := parseSkillMeta(filepath.Join(dir, e.Name(), "SKILL.md"))
		if name == "" {
			name = e.Name()
		}
		out = append(out, name)
	}
	return out
}

// InstalledSkills lists what's already active under ~/.claude/skills.
func InstalledSkills() []Skill {
	entries, err := os.ReadDir(skillsDir())
	if err != nil {
		return nil
	}
	var out []Skill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		skillMD := filepath.Join(skillsDir(), e.Name(), "SKILL.md")
		if _, err := os.Stat(skillMD); err != nil {
			continue
		}
		name, desc := parseSkillMeta(skillMD)
		if name == "" {
			name = e.Name()
		}
		out = append(out, Skill{
			Name:        name,
			Description: desc,
			Path:        filepath.Join(skillsDir(), e.Name()),
			Installed:   true,
		})
	}
	return out
}

// DownloadsSkills finds installable skills in ~/Downloads: directories that
// contain a SKILL.md (searched two levels deep, since archives often unzip into
// a wrapper folder) and .zip files that contain a SKILL.md.
func DownloadsSkills() []Skill {
	var out []Skill
	seen := map[string]bool{}

	// Directories with a SKILL.md.
	_ = filepath.WalkDir(downloadsDir(), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		// Bound the walk: Downloads can be huge.
		if d.IsDir() && depthBelow(downloadsDir(), path) > 2 {
			return filepath.SkipDir
		}
		if d.IsDir() || d.Name() != "SKILL.md" {
			return nil
		}
		dir := filepath.Dir(path)
		if seen[dir] {
			return nil
		}
		seen[dir] = true
		name, desc := parseSkillMeta(path)
		if name == "" {
			name = filepath.Base(dir)
		}
		out = append(out, Skill{Name: name, Description: desc, Path: dir})
		return nil
	})

	// .zip archives that contain a SKILL.md.
	entries, _ := os.ReadDir(downloadsDir())
	for _, e := range entries {
		if e.IsDir() || strings.ToLower(filepath.Ext(e.Name())) != ".zip" {
			continue
		}
		zpath := filepath.Join(downloadsDir(), e.Name())
		if name, desc, ok := peekZipSkill(zpath); ok {
			out = append(out, Skill{Name: name, Description: desc, Path: zpath, IsZip: true})
		}
	}

	// Mark ones already installed so the UI can show "reinstall" vs "install".
	installed := map[string]bool{}
	for _, s := range InstalledSkills() {
		installed[s.Name] = true
	}
	for i := range out {
		out[i].Installed = installed[out[i].Name]
	}
	return out
}

// InstallSkill copies a downloaded skill (folder or zip) into ~/.claude/skills.
// Returns the installed skill name.
func InstallSkill(s Skill) (string, error) {
	if err := os.MkdirAll(skillsDir(), 0o755); err != nil {
		return "", err
	}
	dest := filepath.Join(skillsDir(), safeName(s.Name))

	if s.IsZip {
		if err := unzipInto(s.Path, dest); err != nil {
			return "", err
		}
		// A zip may wrap everything in one top folder; if SKILL.md landed a
		// level down, hoist that folder up so ~/.claude/skills/<name>/SKILL.md.
		flattenIfWrapped(dest)
		return s.Name, nil
	}
	if err := copyTree(s.Path, dest); err != nil {
		return "", err
	}
	return s.Name, nil
}

// CreateSkill authors a new prompt-only skill from the form fields, writing
// ~/.claude/skills/<name>/SKILL.md with YAML frontmatter and the prompt as the
// body. A skill is model-agnostic: it's loaded into whatever session references
// it and runs on whatever model that session uses — so the authored skill can
// draw on the full capabilities and tools of the installed model. We say that
// explicitly in the body so a hand-written skill isn't artificially narrowed.
// Returns the installed skill name.
func CreateSkill(name, description, prompt string) (string, error) {
	if strings.TrimSpace(name) == "" || strings.TrimSpace(description) == "" {
		return "", errSkillIncomplete
	}
	dir := filepath.Join(skillsDir(), safeName(name))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("name: " + name + "\n")
	b.WriteString("description: " + yamlScalar(description) + "\n")
	b.WriteString("---\n\n")
	body := strings.TrimSpace(prompt)
	if body == "" {
		body = "You are a helpful assistant."
	}
	b.WriteString(body + "\n\n")
	b.WriteString("Use the full capabilities of the model running this session — " +
		"reasoning, code, and every tool available to you — to accomplish the task.\n")

	dest := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(dest, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	return name, nil
}

func RemoveSkill(s Skill) error {
	// Only ever remove inside the skills dir — never follow Path out of it.
	dest := filepath.Join(skillsDir(), safeName(s.Name))
	if s.Installed && strings.HasPrefix(s.Path, skillsDir()) {
		dest = s.Path
	}
	return os.RemoveAll(dest)
}

// --- helpers ---

func parseSkillMeta(skillMD string) (name, desc string) {
	data, err := os.ReadFile(skillMD)
	if err != nil {
		return "", ""
	}
	text := string(data)
	// Frontmatter is between the first two "---" lines.
	if !strings.HasPrefix(strings.TrimSpace(text), "---") {
		return "", ""
	}
	parts := strings.SplitN(text, "---", 3)
	if len(parts) < 3 {
		return "", ""
	}
	for _, line := range strings.Split(parts[1], "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(strings.Trim(strings.TrimSpace(val), `"'`))
		switch key {
		case "name":
			name = val
		case "description":
			desc = val
		}
	}
	return name, desc
}

func peekZipSkill(zpath string) (name, desc string, ok bool) {
	r, err := zip.OpenReader(zpath)
	if err != nil {
		return "", "", false
	}
	defer r.Close()
	for _, f := range r.File {
		if filepath.Base(f.Name) != "SKILL.md" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(rc, 1<<20))
		rc.Close()
		tmp := filepath.Join(os.TempDir(), "omni-skill-peek.md")
		_ = os.WriteFile(tmp, data, 0o644)
		n, d := parseSkillMeta(tmp)
		_ = os.Remove(tmp)
		if n == "" {
			n = strings.TrimSuffix(filepath.Base(zpath), ".zip")
		}
		return n, d, true
	}
	return "", "", false
}

func depthBelow(root, path string) int {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return 0
	}
	if rel == "." {
		return 0
	}
	return strings.Count(rel, string(os.PathSeparator)) + 1
}

func copyTree(src, dest string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target, info.Mode())
	})
}

func copyFile(src, dest string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// unzipInto extracts an archive, guarding against Zip Slip (entries that try to
// escape the destination directory with ../).
func unzipInto(zpath, dest string) error {
	r, err := zip.OpenReader(zpath)
	if err != nil {
		return err
	}
	defer r.Close()
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	for _, f := range r.File {
		target := filepath.Join(dest, f.Name)
		if !strings.HasPrefix(target, filepath.Clean(dest)+string(os.PathSeparator)) {
			continue // skip path-traversal entries
		}
		if f.FileInfo().IsDir() {
			_ = os.MkdirAll(target, 0o755)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, f.Mode())
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		out.Close()
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// flattenIfWrapped hoists a single wrapper directory up when the archive put
// SKILL.md one level deep (dest/wrapper/SKILL.md -> dest/SKILL.md).
func flattenIfWrapped(dest string) {
	if _, err := os.Stat(filepath.Join(dest, "SKILL.md")); err == nil {
		return // already at the top
	}
	entries, err := os.ReadDir(dest)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		return
	}
	inner := filepath.Join(dest, entries[0].Name())
	if _, err := os.Stat(filepath.Join(inner, "SKILL.md")); err != nil {
		return
	}
	// Move inner/* up to dest.
	innerEntries, _ := os.ReadDir(inner)
	for _, e := range innerEntries {
		_ = os.Rename(filepath.Join(inner, e.Name()), filepath.Join(dest, e.Name()))
	}
	_ = os.Remove(inner)
}

func safeName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		case r == ' ':
			return '-'
		default:
			return -1
		}
	}, name)
	if name == "" {
		return "skill"
	}
	return name
}
