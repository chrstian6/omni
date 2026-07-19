package main

import (
	"regexp"
	"strings"
)

// The safety layer. Every tool call is classified before it runs:
//
//   block — catastrophic and almost never intended. Hard-denied by the hook;
//           the user isn't even asked. Logged to blocked.log.
//   warn  — legitimately risky. Surfaced with a DANGER badge and always needs
//           an explicit, individual approval — "approve all" never covers it.
//   safe  — everything else. Eligible for approve-all.
//
// This is deliberately conservative: false "warn"s cost one keystroke, a missed
// "block" can wipe a disk. Patterns match on the concrete argument (a shell
// command, a file path) rather than the tool name alone.

type dangerRule struct {
	re     *regexp.Regexp
	reason string
	level  string // riskBlock | riskWarn
}

func rule(level, reason, pattern string) dangerRule {
	return dangerRule{re: regexp.MustCompile(pattern), reason: reason, level: level}
}

// Ordered: the first block wins over any warn, so blocks are listed first.
// Note: recursive `rm` is handled in classifyRm (below), not here, so a plain
// `rm -rf build/` stays a warning while `rm -rf /` is a hard block.
var dangerRules = []dangerRule{
	// ---- hard blocks: destructive to the machine or repo history ----
	rule(riskBlock, "filesystem format", `\bmkfs(\.\w+)?\b`),
	rule(riskBlock, "raw write to a block device", `\bdd\b[^|]*\bof=/dev/(disk|sd|nvme|rdisk)`),
	rule(riskBlock, "overwrite a whole disk device", `>\s*/dev/(disk|sd|nvme|rdisk)\w*`),
	rule(riskBlock, "fork bomb", `:\s*\(\s*\)\s*\{\s*:\s*\|\s*:`),
	rule(riskBlock, "pipe a remote script straight into a shell",
		`\b(curl|wget|fetch)\b[^\n]*\|\s*(sudo\s+)?(ba|z|k|d)?sh\b`),
	rule(riskBlock, "git hard reset losing local commits", `\bgit\s+reset\s+--hard\s+(origin/)?(main|master|HEAD~)`),
	rule(riskBlock, "drop a database or all tables",
		`(?i)\b(drop\s+database|drop\s+table|truncate\s+table|delete\s+from\s+\w+\s*;?\s*$)`),
	rule(riskBlock, "disable the firewall", `(?i)\b(ufw\s+disable|pfctl\s+-d|iptables\s+-F)\b`),
	rule(riskBlock, "make everything world-writable at root", `\bchmod\s+-R\s+777\s+/`),
	rule(riskBlock, "take ownership of the whole filesystem", `\bchown\s+-R\s+[^\s]+\s+/(\s|$)`),
	rule(riskBlock, "shut down or reboot the machine", `\b(shutdown|reboot|halt|poweroff)\b`),

	// ---- warnings: risky but often intended ----
	// (recursive rm is classified in classifyRm, not here)
	rule(riskWarn, "sudo / privilege escalation", `\bsudo\b`),
	rule(riskWarn, "git push", `\bgit\s+push\b`),
	rule(riskWarn, "git force-push (with lease)", `\bgit\s+push\b[^\n]*--force-with-lease`),
	rule(riskWarn, "force-checkout discards local changes", `\bgit\s+(checkout|restore)\b[^\n]*(--force|-f)\b`),
	rule(riskWarn, "clean untracked files", `\bgit\s+clean\b[^\n]*-[a-zA-Z]*f`),
	rule(riskWarn, "package published to a registry", `\b(npm|pnpm|yarn)\s+publish\b`),
	rule(riskWarn, "install a package globally", `\b(npm|pnpm|yarn)\b[^\n]*\s-g\b`),
	rule(riskWarn, "writes to a credentials or secrets file", `(?i)(\.env|\.pem|id_rsa|id_ed25519|credentials|secrets?)\b`),
	rule(riskWarn, "reads a private key or env secret", `(?i)(id_rsa|id_ed25519|\.pem|\.env(\.|$))`),
	rule(riskWarn, "kills processes", `\b(kill|killall|pkill)\b`),
	rule(riskWarn, "moves or overwrites into a system path", `\b(mv|cp)\b[^\n]*\s/(usr|etc|bin|sbin|var|lib)\b`),
	rule(riskWarn, "changes file permissions recursively", `\bchmod\s+-R\b`),
	rule(riskWarn, "terraform/infra apply or destroy", `\bterraform\s+(apply|destroy)\b`),
	rule(riskWarn, "docker prune or system wipe", `\bdocker\s+(system\s+prune|volume\s+rm|rmi)\b`),
	rule(riskWarn, "database migration reset/drop", `(?i)\b(migrate\s+reset|db\s+push\s+--force|prisma\s+migrate\s+reset|drizzle-kit\s+drop)\b`),
}

// classify inspects a tool call and returns its risk level plus the reasons.
func classify(tool string, input map[string]any) (level string, reasons []string) {
	haystacks := riskyStrings(tool, input)
	joined := strings.Join(haystacks, "\n")

	level = riskSafe
	seen := map[string]bool{}
	promote := func(l, reason string) {
		if seen[reason] {
			return
		}
		seen[reason] = true
		reasons = append(reasons, reason)
		if l == riskBlock {
			level = riskBlock
		} else if level != riskBlock {
			level = riskWarn
		}
	}

	for _, r := range dangerRules {
		if r.re.MatchString(joined) {
			promote(r.level, r.reason)
		}
	}

	// git force-push needs argument-order awareness that RE2 can't express, so
	// it's checked here: force-pushing a protected branch is a hard block;
	// any other force-push is a warning.
	if l, reason, ok := classifyGitPush(joined); ok {
		promote(l, reason)
	}

	// recursive rm: block only when the target is catastrophic (root, home,
	// a system dir, or a bare glob); a normal `rm -rf build/` is just a warning.
	if l, reason, ok := classifyRm(joined); ok {
		promote(l, reason)
	}

	return level, reasons
}

var reRecursiveRm = regexp.MustCompile(`\brm\s+((?:-[a-zA-Z]+\s+)*)(.*)`)
var systemRoots = []string{"/", "/usr", "/etc", "/var", "/bin", "/sbin", "/lib",
	"/System", "/Library", "/Applications", "/boot", "/dev", "/proc", "/home", "/opt", "/root"}

func classifyRm(cmd string) (level, reason string, ok bool) {
	for _, line := range strings.Split(cmd, "\n") {
		m := reRecursiveRm.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		flags := m[1]
		if !strings.Contains(flags, "r") && !strings.Contains(flags, "R") &&
			!strings.Contains(line, "--recursive") {
			continue // not a recursive delete
		}
		// Scan the targets after the flags.
		for _, tok := range strings.Fields(m[2]) {
			if strings.HasPrefix(tok, "-") {
				continue // a flag, not a target
			}
			t := strings.Trim(tok, `"'`)
			if isCatastrophicPath(t) {
				return riskBlock, "recursive delete of a protected path (" + t + ")", true
			}
		}
		return riskWarn, "recursive delete", true
	}
	return "", "", false
}

func isCatastrophicPath(t string) bool {
	switch t {
	case "/", "~", "~/", "*", "/*", ".", "..", "./*", "$HOME", "${HOME}", "$HOME/*", "~/*":
		return true
	}
	// Home directory itself, or a system root (possibly with a trailing subpath).
	if t == "$HOME" || strings.HasPrefix(t, "$HOME/") {
		return true
	}
	for _, root := range systemRoots {
		if t == root || strings.HasPrefix(t, root+"/") {
			return true
		}
	}
	return false
}

var protectedBranches = []string{"main", "master", "production", "prod", "release", "develop"}

func classifyGitPush(cmd string) (level, reason string, ok bool) {
	c := strings.ToLower(cmd)
	if !strings.Contains(c, "git push") {
		return "", "", false
	}
	forced := strings.Contains(c, "--force") || regexpWord(c, "-f")
	withLease := strings.Contains(c, "--force-with-lease")
	if !forced {
		return "", "", false
	}
	if withLease {
		return riskWarn, "git force-push (with lease)", true
	}
	for _, b := range protectedBranches {
		if regexpWord(c, b) {
			return riskBlock, "git force-push to protected branch " + b, true
		}
	}
	return riskWarn, "git force-push", true
}

// regexpWord reports whether needle appears as a whole token in s.
func regexpWord(s, needle string) bool {
	re := regexp.MustCompile(`(^|[\s=/])` + regexp.QuoteMeta(needle) + `($|[\s])`)
	return re.MatchString(s)
}

// riskyStrings pulls the fields worth scanning out of a tool's input. Bash
// commands are the main event, but file writes and web fetches carry risk too.
func riskyStrings(tool string, input map[string]any) []string {
	var out []string
	add := func(v any) {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	switch tool {
	case "Bash":
		add(input["command"])
	case "Write", "Edit", "MultiEdit", "NotebookEdit":
		add(input["file_path"])
		add(input["notebook_path"])
	case "Read":
		add(input["file_path"])
	case "WebFetch":
		add(input["url"])
	default:
		// Unknown/MCP tools: scan every string value defensively.
		for _, v := range input {
			add(v)
		}
	}
	return out
}

// summarize renders a compact one-liner of what the tool wants to do, for the
// approval panel. Kept short — the panel shows many at once.
func summarize(tool string, input map[string]any) string {
	switch tool {
	case "Bash":
		if s, ok := input["command"].(string); ok {
			return oneLine(s)
		}
	case "Write", "Edit", "MultiEdit":
		if s, ok := input["file_path"].(string); ok {
			return tool + " " + s
		}
	case "WebFetch":
		if s, ok := input["url"].(string); ok {
			return "fetch " + s
		}
	}
	return tool
}
