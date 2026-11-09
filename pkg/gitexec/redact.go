package gitexec

import (
	"regexp"
	"strings"
)

// redactMask replaces every secret the redactors find.
const redactMask = "***"

// secretFlags take a credential as their next argument (`--token abc`).
var secretFlags = map[string]bool{
	"-u":         true,
	"--user":     true,
	"--password": true,
	"--token":    true,
	"--pat":      true,
}

var secretPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	// URL credentials: https://user:token@host -> https://***@host.
	{regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^/\s@]+@`), "${1}" + redactMask + "@"},
	// Authorization header values, with or without a scheme word.
	{regexp.MustCompile(`(?i)(authorization\s*:\s*)(?:(?:bearer|basic|token)\s+)?\S+`), "${1}" + redactMask},
	// key=value or key: value pairs whose key names a secret.
	{regexp.MustCompile(`(?i)\b((?:private-|x-|access[_-]?)?token|password|passwd|secret|api[_-]?key)(\s*[:=]\s*)\S+`), "${1}${2}" + redactMask},
	// Forge token shapes that can appear anywhere in stderr.
	{regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_-]{20,})`), redactMask},
}

// redactText masks credentials in free text such as stderr or one argument.
func redactText(s string) string {
	for _, p := range secretPatterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}

// redactArgs joins argv for an error message with credentials masked.
func redactArgs(args []string) string {
	out := make([]string, len(args))
	maskNext := false
	for i, a := range args {
		if maskNext {
			out[i] = redactMask
			maskNext = false
			continue
		}
		if name, _, hasValue := strings.Cut(a, "="); hasValue && secretFlags[name] {
			out[i] = name + "=" + redactMask
			continue
		}
		maskNext = secretFlags[a]
		out[i] = redactText(a)
	}
	return strings.Join(out, " ")
}

// redactOutput masks credentials in stderr or stdout before it joins an error.
func redactOutput(msg string) string {
	return redactText(msg)
}
