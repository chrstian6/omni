package main

import (
	"regexp"
	"strings"
)

// choiceOpt is one option the model offered — its original marker ("1", "a") and
// the human label after it.
type choiceOpt struct {
	marker string
	label  string
}

var reChoice = regexp.MustCompile(`^\s*(\d{1,2}|[a-zA-Z])[.)]\s+(.+\S)\s*$`)

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
	var opts []choiceOpt
	lastMatch := -1
	for i, ln := range lines {
		if m := reChoice.FindStringSubmatch(ln); m != nil {
			opts = append(opts, choiceOpt{marker: m[1], label: strings.TrimSpace(m[2])})
			lastMatch = i
		}
	}
	// Need at least two options, and the list must sit near the end of the
	// message (so we don't hijack an incidental list from mid-explanation).
	if len(opts) < 2 || lastMatch < len(lines)-4 {
		return nil
	}
	return opts
}
