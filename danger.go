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
// Deliberately conservative: a false "warn" costs one keystroke, a missed
// "block" can wipe a disk.
//
// Bash commands are classified STRUCTURALLY — parsed into the programs they run
// (see shellparse.go), then judged on program name and arguments. The earlier
// version regexed the whole command string, which could not tell a dangerous
// command from one that merely mentions it: an echo label containing "shutdown"
// or a grep pattern containing "reboot" were hard-blocked, and since a block is
// never shown to the user for approval, the only escape was rewording the
// command. Structural matching is what makes the rules mean what they say.

type dangerRule struct {
	re     *regexp.Regexp
	reason string
	level  string // riskBlock | riskWarn
}

func rule(level, reason, pattern string) dangerRule {
	return dangerRule{re: regexp.MustCompile(pattern), reason: reason, level: level}
}

// pathRules apply to a concrete file path or URL — a Read/Write target, or an
// argument of a shell command. These are matched as text because a path IS the
// argument; there is no program name to key off.
var pathRules = []dangerRule{
	rule(riskWarn, "writes to a credentials or secrets file",
		`(?i)(^|/)(\.env(\.|$)|.*\.pem$|id_rsa|id_ed25519|credentials(\.| |$)|secrets?\.(json|ya?ml|toml))`),
	rule(riskWarn, "reads a private key or env secret",
		`(?i)(^|/)(id_rsa|id_ed25519|.*\.pem$|\.env(\.|$))`),
}

// programBlocks are programs that are catastrophic to RUN, regardless of args.
var programBlocks = map[string]string{
	"shutdown": "shut down or reboot the machine",
	"reboot":   "shut down or reboot the machine",
	"halt":     "shut down or reboot the machine",
	"poweroff": "shut down or reboot the machine",
}

// programWarns are programs that are risky to run in any form.
var programWarns = map[string]string{
	"kill":    "kills processes",
	"killall": "kills processes",
	"pkill":   "kills processes",
}

var shellNames = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "ksh": true, "dash": true, "fish": true,
}

var dbClients = map[string]bool{
	"psql": true, "mysql": true, "mariadb": true, "sqlite3": true, "mongo": true,
	"mongosh": true, "redis-cli": true, "cockroach": true, "clickhouse-client": true,
}

var (
	reForkBomb = regexp.MustCompile(`:\s*\(\s*\)\s*\{\s*:\s*\|\s*:`)
	reDevWrite = regexp.MustCompile(`>\s*/dev/(disk|sd|nvme|rdisk)\w*`)
	reSQLDrop  = regexp.MustCompile(`(?i)\b(drop\s+database|drop\s+table|truncate\s+table|delete\s+from\s+\w+\s*;?\s*$)`)
	reMkfs     = regexp.MustCompile(`^mkfs(\.\w+)?$`)
)

// classify inspects a tool call and returns its risk level plus the reasons.
func classify(tool string, input map[string]any) (level string, reasons []string) {
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

	if tool == "Bash" {
		cmdline, _ := input["command"].(string)
		classifyShell(cmdline, promote)
		// Paths mentioned as arguments still matter (reading a private key), and
		// are matched on the argument text.
		for _, c := range ParseShell(cmdline) {
			for _, a := range c.argTexts() {
				matchPathRules(a, promote)
			}
		}
		return level, reasons
	}

	// Non-shell tools: the risky value is a path or URL, matched as text.
	for _, s := range riskyStrings(tool, input) {
		matchPathRules(s, promote)
	}
	return level, reasons
}

func matchPathRules(s string, promote func(string, string)) {
	for _, r := range pathRules {
		if r.re.MatchString(s) {
			promote(r.level, r.reason)
		}
	}
}

// classifyShell walks the actual commands a line runs.
func classifyShell(cmdline string, promote func(string, string)) {
	if cmdline == "" {
		return
	}
	// Syntax-level hazards that aren't a program: a fork bomb is punctuation, and
	// a redirect onto a raw device is an operator, not an argument.
	if reForkBomb.MatchString(cmdline) {
		promote(riskBlock, "fork bomb")
	}

	cmds := ParseShell(cmdline)
	for i, c := range cmds {
		name := baseName(c.prog)

		// A redirect is checked against the command's own text, so a quoted
		// string elsewhere in the line can't manufacture one.
		if reDevWrite.MatchString(c.raw) {
			promote(riskBlock, "overwrite a whole disk device")
		}
		if c.sudo {
			promote(riskWarn, "sudo / privilege escalation")
		}
		if reason, ok := programBlocks[name]; ok {
			promote(riskBlock, reason)
		}
		if reason, ok := programWarns[name]; ok {
			promote(riskWarn, reason)
		}
		if reMkfs.MatchString(name) {
			promote(riskBlock, "filesystem format")
		}

		args := c.argTexts()
		switch {
		case name == "dd":
			for _, a := range args {
				if strings.HasPrefix(a, "of=/dev/") && looksLikeWholeDisk(strings.TrimPrefix(a, "of=")) {
					promote(riskBlock, "raw write to a block device")
				}
			}
		case name == "rm":
			classifyRmCmd(c, promote)
		case name == "chmod":
			if c.hasFlag("--recursive", "R") {
				promote(riskWarn, "changes file permissions recursively")
				for i, a := range args {
					if a == "777" && i+1 < len(args) && isCatastrophicPath(args[i+1]) {
						promote(riskBlock, "make everything world-writable at root")
					}
				}
			}
		case name == "chown":
			if c.hasFlag("--recursive", "R") {
				for _, a := range args[1:] {
					if isCatastrophicPath(a) {
						promote(riskBlock, "take ownership of the whole filesystem")
					}
				}
			}
		case name == "git":
			classifyGitCmd(c, promote)
		case name == "npm" || name == "pnpm" || name == "yarn":
			if hasWord(args, "publish") {
				promote(riskWarn, "package published to a registry")
			}
			if c.hasFlag("--global", "g") {
				promote(riskWarn, "install a package globally")
			}
		case name == "docker":
			if (hasWord(args, "system") && hasWord(args, "prune")) ||
				(hasWord(args, "volume") && hasWord(args, "rm")) || hasWord(args, "rmi") {
				promote(riskWarn, "docker prune or system wipe")
			}
		case name == "terraform":
			if hasWord(args, "apply") || hasWord(args, "destroy") {
				promote(riskWarn, "terraform/infra apply or destroy")
			}
		case name == "ufw" && hasWord(args, "disable"),
			name == "pfctl" && hasWord(args, "-d"),
			name == "iptables" && hasWord(args, "-F"):
			promote(riskBlock, "disable the firewall")
		case name == "mv" || name == "cp":
			for _, a := range args {
				if isSystemPath(a) {
					promote(riskWarn, "moves or overwrites into a system path")
				}
			}
		}

		// Migration resets, however they're invoked (npx prisma …, drizzle-kit …).
		joined := name + " " + strings.Join(args, " ")
		if regexp.MustCompile(`(?i)\b(migrate\s+reset|db\s+push\s+--force|prisma\s+migrate\s+reset|drizzle-kit\s+drop)\b`).
			MatchString(joined) {
			promote(riskWarn, "database migration reset/drop")
		}

		// SQL is only executable when handed to something that runs SQL, so a
		// grep for "DROP TABLE" in a schema file is not a database wipe.
		if dbClients[name] {
			for _, a := range args {
				if reSQLDrop.MatchString(a) {
					promote(riskBlock, "drop a database or all tables")
				}
			}
		}

		// A remote script piped into a shell: the download and the shell are
		// separate commands, so this is a property of the pipeline.
		if (name == "curl" || name == "wget" || name == "fetch") && i+1 < len(cmds) {
			next := cmds[i+1]
			if next.stdin && (shellNames[baseName(next.prog)] || next.sudo && shellNames[baseName(next.prog)]) {
				promote(riskBlock, "pipe a remote script straight into a shell")
			}
		}
	}
}

func hasWord(args []string, w string) bool {
	for _, a := range args {
		if a == w {
			return true
		}
	}
	return false
}

func looksLikeWholeDisk(p string) bool {
	return regexp.MustCompile(`^/dev/(disk|sd|nvme|rdisk)\w*$`).MatchString(p)
}

// classifyRmCmd judges a delete by ITS OWN targets. Scoping to this command is
// the point: the old scan ran to the end of the line, so `rm -rf build && du -sh .`
// picked up du's "." and hard-blocked a harmless delete.
func classifyRmCmd(c shCmd, promote func(string, string)) {
	if !c.hasFlag("--recursive", "r", "R") {
		return
	}
	for _, a := range c.argTexts() {
		if strings.HasPrefix(a, "-") {
			continue
		}
		if isCatastrophicPath(a) {
			promote(riskBlock, "recursive delete of a protected path ("+a+")")
			return
		}
	}
	promote(riskWarn, "recursive delete")
}

var protectedBranches = []string{"main", "master", "production", "prod", "release", "develop"}

func classifyGitCmd(c shCmd, promote func(string, string)) {
	args := c.argTexts()
	if len(args) == 0 {
		return
	}
	switch args[0] {
	case "push":
		promote(riskWarn, "git push")
		forced, withLease := false, false
		for _, a := range args {
			switch {
			case a == "--force-with-lease" || strings.HasPrefix(a, "--force-with-lease="):
				forced, withLease = true, true
			case a == "--force" || a == "-f":
				forced = true
			}
		}
		if !forced {
			return
		}
		if withLease {
			promote(riskWarn, "git force-push (with lease)")
			return
		}
		for _, a := range args {
			for _, b := range protectedBranches {
				if a == b {
					promote(riskBlock, "git force-push to protected branch "+b)
					return
				}
			}
		}
		promote(riskWarn, "git force-push")
	case "reset":
		if hasWord(args, "--hard") {
			for _, a := range args {
				t := strings.TrimPrefix(a, "origin/")
				if t == "main" || t == "master" || strings.HasPrefix(a, "HEAD~") {
					promote(riskBlock, "git hard reset losing local commits")
					return
				}
			}
		}
	case "checkout", "restore":
		if c.hasFlag("--force", "f") {
			promote(riskWarn, "force-checkout discards local changes")
		}
	case "clean":
		if c.hasFlag("--force", "f") {
			promote(riskWarn, "clean untracked files")
		}
	}
}

var systemRoots = []string{"/", "/usr", "/etc", "/var", "/bin", "/sbin", "/lib",
	"/System", "/Library", "/Applications", "/boot", "/dev", "/proc", "/home", "/opt", "/root"}

func isCatastrophicPath(t string) bool {
	t = strings.Trim(t, `"'`)
	switch t {
	case "/", "~", "~/", "*", "/*", ".", "..", "./*", "$HOME", "${HOME}", "$HOME/*", "~/*":
		return true
	}
	if strings.HasPrefix(t, "$HOME/") || strings.HasPrefix(t, "${HOME}/") || strings.HasPrefix(t, "~/") {
		// A specific path under home is a normal delete; the home root is not.
		return strings.TrimRight(t, "/") == "$HOME" || strings.TrimRight(t, "/") == "~"
	}
	for _, root := range systemRoots {
		if t == root || (root != "/" && strings.HasPrefix(t, root+"/")) {
			return true
		}
	}
	return false
}

func isSystemPath(t string) bool {
	for _, root := range []string{"/usr", "/etc", "/bin", "/sbin", "/var", "/lib"} {
		if t == root || strings.HasPrefix(t, root+"/") {
			return true
		}
	}
	return false
}

// riskyStrings pulls the fields worth scanning out of a non-shell tool's input.
func riskyStrings(tool string, input map[string]any) []string {
	var out []string
	add := func(v any) {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	switch tool {
	case "Write", "Edit", "MultiEdit", "NotebookEdit":
		add(input["file_path"])
		add(input["notebook_path"])
	case "Read":
		add(input["file_path"])
	case "WebFetch":
		add(input["url"])
	default:
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
