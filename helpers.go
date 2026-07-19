package main

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

func itoa(n int) string { return strconv.Itoa(n) }

func trimSpace(s string) string { return strings.TrimSpace(s) }

func plural(n int, one, many string) string {
	if n == 1 {
		return itoa(n) + " " + one
	}
	return itoa(n) + " " + many
}

// padRight pads to a visible width, counting terminal cells rather than bytes
// so wide/emoji glyphs and multibyte runes line up correctly.
func padRight(s string, width int) string {
	w := lipgloss.Width(s)
	if w >= width {
		return s
	}
	return s + strings.Repeat(" ", width-w)
}

func truncate(s string, max int) string {
	if max <= 0 {
		return s
	}
	return ansi.Truncate(s, max, "…")
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// expandHome resolves a leading ~ to the user's home directory.
func expandHome(p string) string {
	if p == "~" {
		return homeDir()
	}
	if strings.HasPrefix(p, "~/") {
		return homeDir() + p[1:]
	}
	return p
}

// displayText turns raw transcript text into something that reads cleanly in the
// panel: HTML entities decoded, markdown punctuation removed. Emoji and every
// other rune pass through untouched — we only strip the syntax, never content.
func displayText(s string) string {
	return stripMarkdown(unescapeEntities(s))
}

// unescapeEntities decodes the handful of HTML entities that show up in
// transcripts (Claude escapes < > & in code it writes back), so "&gt;" reads as
// ">" instead of leaking the entity.
func unescapeEntities(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	r := strings.NewReplacer(
		"&lt;", "<", "&gt;", ">", "&amp;", "&",
		"&quot;", `"`, "&#39;", "'", "&apos;", "'", "&nbsp;", " ",
	)
	return r.Replace(s)
}

var (
	mdBold       = regexp.MustCompile(`\*\*([^*]+)\*\*|__([^_]+)__`)
	mdItalic     = regexp.MustCompile(`(^|[\s(])[*_]([^*_\n]+)[*_]([\s).,;:!?]|$)`)
	mdCode       = regexp.MustCompile("`+([^`]+)`+")
	mdLink       = regexp.MustCompile(`!?\[([^\]]+)\]\([^)]*\)`)
	mdHeading    = regexp.MustCompile(`(?m)^\s{0,3}#{1,6}\s+`)
	mdListBullet = regexp.MustCompile(`(?m)^(\s*)[-*+]\s+`)
)

// stripMarkdown removes the emphasis/heading/code/link *syntax* while keeping the
// words. It is deliberately light — enough that "**Reserve consistency**" reads
// as "Reserve consistency" and `code` as code, without a full parser.
func stripMarkdown(s string) string {
	s = mdCode.ReplaceAllString(s, "$1")
	s = mdBold.ReplaceAllString(s, "$1$2")
	s = mdItalic.ReplaceAllString(s, "$1$2$3")
	s = mdLink.ReplaceAllString(s, "$1")
	s = mdHeading.ReplaceAllString(s, "")
	s = mdListBullet.ReplaceAllString(s, "$1• ")
	return s
}

// prettyModel turns a raw model id into a short, human label — "claude-opus-4-8"
// → "Opus 4.8", "claude-haiku-4-5-20251001" → "Haiku 4.5". It finds the family
// word and joins the version numbers, dropping the "claude-" prefix, any bracket
// suffix like "[1m]", and the trailing yyyymmdd date stamp. Unknown shapes fall
// back to the cleaned id so nothing is ever lost.
func prettyModel(id string) string {
	if id == "" {
		return ""
	}
	s := strings.ToLower(strings.TrimSpace(id))
	if i := strings.IndexByte(s, '['); i >= 0 { // drop "[1m]" and the like
		s = s[:i]
	}
	s = strings.TrimPrefix(s, "claude-")
	family := ""
	var nums []string
	for _, tok := range strings.Split(s, "-") {
		switch tok {
		case "opus", "sonnet", "haiku", "fable":
			family = strings.ToUpper(tok[:1]) + tok[1:]
		default:
			if len(tok) == 8 && isDigits(tok) { // yyyymmdd date stamp
				continue
			}
			if tok != "" && isDigits(tok) {
				nums = append(nums, tok)
			}
		}
	}
	if family == "" {
		return strings.TrimSpace(id)
	}
	if len(nums) == 0 {
		return family
	}
	return family + " " + strings.Join(nums, ".")
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func indent(s string, n int) string {
	pad := strings.Repeat(" ", n)
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = pad + lines[i]
	}
	return strings.Join(lines, "\n")
}

func strikethrough(s string) string {
	return lipgloss.NewStyle().Strikethrough(true).Render(s)
}

var ansiRE = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

// spacer returns the padding that pushes left and right footer segments apart.
func spacer(total int, left, right string) string {
	gap := total - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return strings.Repeat(" ", gap)
}

// clampAnsi truncates to a visible-cell width, preserving color codes and
// without adding an ellipsis (used to fit panel columns).
func clampAnsi(s string, w int) string {
	if w <= 0 {
		return ""
	}
	return ansi.Truncate(s, w, "")
}

// padRightAnsi pads to a visible width; padRight already counts cells not bytes.
func padRightAnsi(s string, w int) string { return padRight(s, w) }

// wrapPlain word-wraps plain text to width w, returning one string per line.
func wrapPlain(s string, w int) []string {
	if w < 4 {
		w = 4
	}
	var out []string
	for _, para := range strings.Split(s, "\n") {
		words := strings.Fields(para)
		if len(words) == 0 {
			out = append(out, "")
			continue
		}
		line := ""
		for _, word := range words {
			// A single over-long word (a URL, a path) is hard-split.
			for lipgloss.Width(word) > w {
				if line != "" {
					out = append(out, line)
					line = ""
				}
				out = append(out, word[:w])
				word = word[w:]
			}
			if line == "" {
				line = word
			} else if lipgloss.Width(line)+1+lipgloss.Width(word) <= w {
				line += " " + word
			} else {
				out = append(out, line)
				line = word
			}
		}
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// wrapPlainStyled wraps then applies a style to each line.
func wrapPlainStyled(s string, w int, st lipgloss.Style) []string {
	lines := wrapPlain(s, w)
	for i := range lines {
		lines[i] = st.Render(lines[i])
	}
	return lines
}
