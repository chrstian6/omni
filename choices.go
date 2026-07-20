package main

import (
	"regexp"
	"strings"
)

// choiceOpt is one option the model offered — its original marker ("1", "a"),
// the heading label after it, and an optional one-line description (the indented
// text Claude often puts under each option).
type choiceOpt struct {
	marker string
	label  string
	desc   string
}

// reChoice matches an option line like "1. Merge, then next gap", tolerating a
// leading selection cursor ("❯ 1.", "› 1)") — the menu's highlighted row carries
// one, and without this the currently-selected option was silently dropped.
var reChoice = regexp.MustCompile(`^\s*(?:[❯›»▶▸→*·•>\-]\s+)?(\d{1,2}|[a-zA-Z])[.)]\s+(.+\S)\s*$`)

// detectChoices looks at the most recent Claude turn and, if it ends by offering
// a numbered/lettered list of options, returns them so the UI can present a
// navigable select instead of the free-text prompt. Returns nil when the last
// speaker was the user (nothing to choose) or no clean option list is present.
func detectChoices(convo []convoTurn) []choiceOpt {
	text := ""
	for i := len(convo) - 1; i >= 0; i-- {
		switch convo[i].role {
		case "claude":
			if convo[i].text != "" {
				text = convo[i].text
			}
		case "you":
			return nil // user spoke last — not waiting on a choice
		}
		if text != "" {
			break
		}
	}
	if text == "" {
		return nil
	}

	lines := strings.Split(text, "\n")
	// Record each option with the line it sat on, so we can grab the description
	// lines that follow it (up to the next option).
	type hit struct {
		line int
		opt  choiceOpt
	}
	var hits []hit
	for i, ln := range lines {
		if m := reChoice.FindStringSubmatch(ln); m != nil {
			hits = append(hits, hit{line: i, opt: choiceOpt{marker: m[1], label: strings.TrimSpace(m[2])}})
		}
	}
	if len(hits) < 2 {
		return nil
	}

	// The list must sit near the end of the message (so an incidental list from
	// mid-explanation isn't hijacked) — measured against the last real line,
	// ignoring trailing blanks and divider rules.
	end := len(lines)
	for end > 0 && (strings.TrimSpace(lines[end-1]) == "" || isDivider(lines[end-1])) {
		end--
	}
	if hits[len(hits)-1].line < end-6 {
		return nil
	}

	// Attach the first substantive line under each option as its description.
	for h := range hits {
		stop := len(lines)
		if h+1 < len(hits) {
			stop = hits[h+1].line
		}
		for j := hits[h].line + 1; j < stop; j++ {
			s := strings.TrimSpace(lines[j])
			if s == "" || isDivider(lines[j]) {
				continue
			}
			hits[h].opt.desc = s
			break
		}
	}

	opts := make([]choiceOpt, len(hits))
	for i := range hits {
		opts[i] = hits[i].opt
	}
	return opts
}

// choicesKey fingerprints a menu by its options, so the same menu can be
// recognized across refreshes while a different one is treated as new. Empty
// for an empty menu, which never matches.
func choicesKey(opts []choiceOpt) string {
	if len(opts) == 0 {
		return ""
	}
	var b strings.Builder
	for _, o := range opts {
		b.WriteString(o.marker)
		b.WriteByte('.')
		b.WriteString(o.label)
		b.WriteByte('\n')
	}
	return b.String()
}

// isDivider reports whether a line is just a horizontal rule (box-drawing or
// dashes) — the separators Claude Code draws around a choice menu.
func isDivider(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 3 {
		return false
	}
	return strings.Trim(s, "─—-=_·• ") == ""
}
