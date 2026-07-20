package main

import "strings"

// A minimal shell splitter, so the safety layer can reason about what a command
// RUNS rather than what it merely mentions.
//
// The classifier used to regex over the whole command string, which cannot tell
// `shutdown` the program from "shutdown" the word inside an echo label or a grep
// pattern. Both are hard blocks the user can't override, so a false positive is
// unappealable — the only escape is rewording the command, which is exactly the
// wrong incentive for a safety layer. It also let `rm`'s target scan run past the
// end of the rm command into later ones, so `rm -rf osm && … && du -sh .` blocked
// on the "." belonging to du.
//
// This is not a POSIX shell parser and doesn't try to be. It understands quoting,
// escapes, separators, pipelines and command substitution — enough to answer
// "which programs does this run, with which arguments", which is the question the
// rules actually need.

// shTok is one argument, remembering whether it came from inside quotes.
// Quoting matters: a quoted word is DATA (`echo "reboot"`) unless it is handed
// to something that executes it (`sh -c "reboot"`), which is why shCmd.execArg
// exists.
type shTok struct {
	text   string
	quoted bool
}

// shCmd is one simple command in a pipeline or sequence.
type shCmd struct {
	prog  string  // argv[0] after stripping sudo/env-style wrappers
	args  []shTok // everything after prog
	raw   string  // the original text of this command, for redirect checks
	sudo  bool    // ran under sudo (or doas)
	stdin bool    // receives piped stdin from the previous command
}

// argTexts is the plain argument strings, quoting discarded.
func (c shCmd) argTexts() []string {
	out := make([]string, 0, len(c.args))
	for _, a := range c.args {
		out = append(out, a.text)
	}
	return out
}

// hasFlag reports whether any argument is exactly one of the given flags, or a
// short-flag bundle containing one of the single letters (so -rf matches "r").
func (c shCmd) hasFlag(long string, letters ...string) bool {
	for _, a := range c.argTexts() {
		if a == long {
			return true
		}
		if len(a) > 1 && a[0] == '-' && !strings.HasPrefix(a, "--") {
			for _, l := range letters {
				if strings.Contains(a[1:], l) {
					return true
				}
			}
		}
	}
	return false
}

// execArg returns the argument that this command will itself execute as shell
// code, if any — `sh -c "…"`, `bash -c "…"`, `eval "…"`. Quoted text there is
// code, not data, so the classifier must recurse into it.
func (c shCmd) execArg() (string, bool) {
	switch baseName(c.prog) {
	case "sh", "bash", "zsh", "ksh", "dash", "fish":
		args := c.argTexts()
		for i, a := range args {
			if a == "-c" && i+1 < len(args) {
				return args[i+1], true
			}
		}
	case "eval":
		return strings.Join(c.argTexts(), " "), true
	}
	return "", false
}

// wrappers run another program and should be transparent to classification —
// `sudo rm -rf /` is an rm, not a sudo.
var cmdWrappers = map[string]bool{
	"sudo": true, "doas": true, "env": true, "nohup": true,
	"command": true, "time": true, "nice": true, "xargs": true, "exec": true,
}

// ParseShell splits a command line into the simple commands it runs, following
// pipelines, sequences and command substitution.
func ParseShell(line string) []shCmd {
	var out []shCmd
	for _, seg := range splitSegments(line) {
		if c, ok := buildCmd(seg.text, seg.piped); ok {
			out = append(out, c)
			// Code passed to a shell is code: classify it too, or `sh -c "rm -rf /"`
			// would look like a harmless invocation of sh.
			if inner, ok := c.execArg(); ok && strings.TrimSpace(inner) != "" {
				out = append(out, ParseShell(inner)...)
			}
		}
		// Command substitution runs its contents.
		for _, sub := range seg.subs {
			out = append(out, ParseShell(sub)...)
		}
	}
	return out
}

type segment struct {
	text  string
	piped bool
	subs  []string // $( … ) and ` … ` contents
}

// splitSegments breaks a line on unquoted separators, collecting command
// substitutions as it goes.
func splitSegments(line string) []segment {
	var segs []segment
	var cur strings.Builder
	var subs []string
	piped := false

	flush := func(nextPiped bool) {
		if strings.TrimSpace(cur.String()) != "" || len(subs) > 0 {
			segs = append(segs, segment{text: cur.String(), piped: piped, subs: subs})
		}
		cur.Reset()
		subs = nil
		piped = nextPiped
	}

	runes := []rune(line)
	var quote rune // 0, '\'' or '"'
	for i := 0; i < len(runes); i++ {
		r := runes[i]

		if quote != 0 {
			if r == '\\' && quote == '"' && i+1 < len(runes) {
				cur.WriteRune(r)
				i++
				cur.WriteRune(runes[i])
				continue
			}
			// Command substitution inside double quotes still executes.
			if quote == '"' && r == '$' && i+1 < len(runes) && runes[i+1] == '(' {
				body, next := readBalanced(runes, i+2, '(', ')')
				subs = append(subs, body)
				i = next
				continue
			}
			if r == quote {
				quote = 0
			}
			cur.WriteRune(r)
			continue
		}

		switch r {
		case '\\':
			if i+1 < len(runes) {
				cur.WriteRune(r)
				i++
				cur.WriteRune(runes[i])
			}
			continue
		case '\'', '"':
			quote = r
			cur.WriteRune(r)
			continue
		case '$':
			if i+1 < len(runes) && runes[i+1] == '(' {
				body, next := readBalanced(runes, i+2, '(', ')')
				subs = append(subs, body)
				i = next
				continue
			}
		case '`':
			body, next := readUntil(runes, i+1, '`')
			subs = append(subs, body)
			i = next
			continue
		case '|':
			if i+1 < len(runes) && runes[i+1] == '|' {
				i++
				flush(false)
			} else {
				flush(true) // the next command reads this one's stdout
			}
			continue
		case '&':
			if i+1 < len(runes) && runes[i+1] == '&' {
				i++
			}
			flush(false)
			continue
		case ';', '\n':
			flush(false)
			continue
		}
		cur.WriteRune(r)
	}
	flush(false)
	return segs
}

// readBalanced reads until the matching close rune, honouring nesting.
func readBalanced(runes []rune, start int, open, close rune) (string, int) {
	depth := 1
	var sb strings.Builder
	i := start
	for ; i < len(runes); i++ {
		switch runes[i] {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return sb.String(), i
			}
		}
		sb.WriteRune(runes[i])
	}
	return sb.String(), i - 1
}

func readUntil(runes []rune, start int, stop rune) (string, int) {
	var sb strings.Builder
	i := start
	for ; i < len(runes); i++ {
		if runes[i] == stop {
			return sb.String(), i
		}
		sb.WriteRune(runes[i])
	}
	return sb.String(), i - 1
}

// buildCmd turns one segment into a command, stripping leading environment
// assignments and wrapper programs.
func buildCmd(seg string, piped bool) (shCmd, bool) {
	toks := tokenize(seg)
	c := shCmd{raw: seg, stdin: piped}

	for len(toks) > 0 {
		head := toks[0]
		// FOO=bar prefixes are environment, not the program.
		if !head.quoted && strings.Contains(head.text, "=") &&
			!strings.HasPrefix(head.text, "-") &&
			strings.Index(head.text, "=") > 0 &&
			isEnvAssignment(head.text) {
			toks = toks[1:]
			continue
		}
		name := baseName(head.text)
		if cmdWrappers[name] {
			if name == "sudo" || name == "doas" {
				c.sudo = true
			}
			toks = toks[1:]
			// Skip sudo/env flags before the real program.
			for len(toks) > 0 && strings.HasPrefix(toks[0].text, "-") {
				toks = toks[1:]
			}
			continue
		}
		break
	}
	if len(toks) == 0 {
		return c, false
	}
	c.prog = toks[0].text
	c.args = toks[1:]
	return c, true
}

// isEnvAssignment reports whether a token looks like NAME=value.
func isEnvAssignment(s string) bool {
	i := strings.Index(s, "=")
	if i <= 0 {
		return false
	}
	for j, r := range s[:i] {
		isAlpha := r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		isDigit := r >= '0' && r <= '9'
		if !isAlpha && !(isDigit && j > 0) {
			return false
		}
	}
	return true
}

// tokenize splits a single command into words, unwrapping quotes but recording
// that they were there.
func tokenize(s string) []shTok {
	var out []shTok
	var cur strings.Builder
	started, quoted := false, false
	var quote rune

	flush := func() {
		if started {
			out = append(out, shTok{text: cur.String(), quoted: quoted})
		}
		cur.Reset()
		started, quoted = false, false
	}

	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if quote != 0 {
			if r == '\\' && quote == '"' && i+1 < len(runes) {
				i++
				cur.WriteRune(runes[i])
				continue
			}
			if r == quote {
				quote = 0
				continue
			}
			cur.WriteRune(r)
			continue
		}
		switch r {
		case '\\':
			if i+1 < len(runes) {
				i++
				cur.WriteRune(runes[i])
				started = true
			}
			continue
		case '\'', '"':
			quote = r
			started, quoted = true, true
			continue
		case ' ', '\t':
			flush()
			continue
		}
		cur.WriteRune(r)
		started = true
	}
	flush()
	return out
}
