package relay

import (
	"path"
	"regexp"
	"strings"
	"unicode"

	"meshra/internal/model"
)

// This is a pre-prompt guard for recognizable dangerous actions, not a shell
// parser or a security boundary. Confinement remains mandatory for commands.
var forkBombPattern = regexp.MustCompile(`(?s)(?:\b([a-zA-Z_][a-zA-Z_0-9]*)|:)\s*\(\s*\)\s*\{[^}]*\|[^}]*&`)
var diskPathPattern = regexp.MustCompile(`^/dev/(?:[shv]d[a-z]+[0-9]*|xvd[a-z]+[0-9]*|nvme[0-9]+n[0-9]+(?:p[0-9]+)?|mmcblk[0-9]+(?:p[0-9]+)?|loop[0-9]+|disk(?:/|$))`)

func denyToolAction(args any) error {
	switch q := args.(type) {
	case *toolWriteArgs:
		if protectedToolPath(q.Path) {
			return model.Err(403, "hardline deny: writes to protected secret paths are forbidden")
		}
		if diskPathPattern.MatchString(path.Clean(q.Path)) {
			return model.Err(403, "hardline deny: raw disk writes are forbidden")
		}
	case *toolCommandArgs:
		var words []string
		if q.Shell != "" {
			if forkBombPattern.MatchString(q.Shell) {
				return model.Err(403, "hardline deny: fork bomb command is forbidden")
			}
			words = denyShellWords(strings.ReplaceAll(q.Shell, "${HOME}", "$HOME"))
			for _, word := range words {
				if dangerousRootRemoval(denyShellWords(word)) {
					return model.Err(403, "hardline deny: recursive removal of the filesystem root is forbidden")
				}
			}
		} else {
			words = q.Argv
			// Interpreters can receive shell text as an argument. Inspect it too,
			// without treating ordinary argv boundaries as shell syntax.
			for _, arg := range q.Argv {
				if forkBombPattern.MatchString(arg) || dangerousRootRemoval(denyShellWords(arg)) {
					return model.Err(403, "hardline deny: catastrophic command is forbidden")
				}
			}
		}
		if dangerousRootRemoval(words) {
			return model.Err(403, "hardline deny: recursive removal of the filesystem root is forbidden")
		}
		// Only a small set of plainly read-only commands is exempt. Unknown
		// programs may write their path arguments, even without a redirection.
		readOnly := denyReadOnlyCommand(words)
		for _, word := range words {
			for _, candidate := range denyPathCandidates(word) {
				if !readOnly && protectedToolPath(candidate) {
					return model.Err(403, "hardline deny: writes to protected secret paths are forbidden")
				}
				if !readOnly && diskPathPattern.MatchString(path.Clean(candidate)) {
					return model.Err(403, "hardline deny: raw disk writes are forbidden")
				}
			}
		}
	}
	return nil
}

// Shell quoting and escaping are folded for common literal commands only;
// expansions, substitutions and shell grammar are deliberately not evaluated.
func denyShellWords(s string) []string {
	var words []string
	var b strings.Builder
	var quote rune
	escaped := false
	flush := func() {
		if b.Len() > 0 {
			words = append(words, b.String())
			b.Reset()
		}
	}
	for _, r := range s {
		if escaped {
			b.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				b.WriteRune(r)
			}
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			continue
		}
		if unicode.IsSpace(r) {
			flush()
			continue
		}
		if strings.ContainsRune(";|&<>(){}", r) {
			flush()
			words = append(words, string(r))
			continue
		}
		b.WriteRune(r)
	}
	flush()
	return words
}

func dangerousRootRemoval(words []string) bool {
	for i, word := range words {
		if path.Base(word) != "rm" {
			continue
		}
		recursive, root := false, false
		for _, arg := range words[i+1:] {
			if strings.ContainsAny(arg, ";|&<>(){}") {
				break
			}
			if arg == "--recursive" || strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.ContainsAny(arg[1:], "rR") {
				recursive = true
			}
			p := path.Clean(arg)
			if p == "/" || p == "/*" || p == "/**" {
				root = true
			}
		}
		if recursive && root {
			return true
		}
	}
	return false
}

func denyReadOnlyCommand(words []string) bool {
	if len(words) == 0 {
		return false
	}
	switch path.Base(words[0]) {
	case "cat", "head", "tail", "less", "more", "stat", "ls", "readlink", "realpath", "wc", "sha256sum", "md5sum":
	default:
		return false
	}
	for _, word := range words[1:] {
		if strings.ContainsAny(word, ";|&<>(){}") {
			return false
		}
	}
	return true
}

func denyPathCandidates(word string) []string {
	// Recognize option assignments (such as dd of=/dev/sda) and paths in
	// quoted interpreter snippets as well as ordinary standalone arguments.
	return strings.FieldsFunc(word, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune("=\"'`;|&<>(){}", r)
	})
}

func protectedToolPath(p string) bool {
	p = strings.ReplaceAll(p, "${HOME}", "~")
	p = strings.ReplaceAll(p, "$HOME", "~")
	p = path.Clean(p)
	parts := strings.Split(p, "/")
	for i, part := range parts {
		switch part {
		case ".ssh", ".aws", ".azure", ".gnupg", ".meshra":
			return true
		case "automation.key":
			return true
		case ".config":
			if i+1 < len(parts) && (parts[i+1] == "gcloud" || parts[i+1] == "meshra") {
				return true
			}
		case ".local":
			if i+2 < len(parts) && parts[i+1] == "share" && parts[i+2] == "meshra" {
				return true
			}
		}
	}
	return false
}
