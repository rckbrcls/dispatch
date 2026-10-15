package sandbox

import (
	"os"
	"strings"
)

// allowedExact is the set of environment variable names always preserved.
var allowedExact = map[string]bool{
	"PATH":    true,
	"HOME":    true,
	"USER":    true,
	"LOGNAME": true,
	"SHELL":   true,
	"TMPDIR":  true,
	"LANG":    true,
	"TERM":    true,
}

// allowedPrefixes are prefixes of environment variable names always preserved.
var allowedPrefixes = []string{
	"LC_",
	"XDG_",
	"DISPATCH_",
}

// Sanitize filters src retaining only allowlisted environment variables:
// PATH, HOME, USER, LOGNAME, SHELL, TMPDIR, LANG, TERM, prefixes LC_*, XDG_*,
// DISPATCH_*, and names listed in DISPATCH_ENV_ALLOW (comma-separated).
// Extra variables in "KEY=VALUE" form can be appended/overridden.
func Sanitize(src []string, extra ...string) []string {
	allowExtra := map[string]bool{}

	for _, entry := range src {
		if k, v, ok := strings.Cut(entry, "="); ok && k == "DISPATCH_ENV_ALLOW" {
			for _, name := range strings.Split(v, ",") {
				if n := strings.TrimSpace(name); n != "" {
					allowExtra[n] = true
				}
			}
		}
	}
	if len(allowExtra) == 0 {
		if v := os.Getenv("DISPATCH_ENV_ALLOW"); v != "" {
			for _, name := range strings.Split(v, ",") {
				if n := strings.TrimSpace(name); n != "" {
					allowExtra[n] = true
				}
			}
		}
	}

	extraKeys := map[string]bool{}
	for _, e := range extra {
		if k, _, ok := strings.Cut(e, "="); ok {
			extraKeys[k] = true
		}
	}

	var out []string
	for _, entry := range src {
		k, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if extraKeys[k] {
			continue
		}
		if isAllowed(k, allowExtra) {
			out = append(out, entry)
		}
	}
	out = append(out, extra...)
	return out
}

func isAllowed(key string, allowExtra map[string]bool) bool {
	if allowedExact[key] {
		return true
	}
	for _, p := range allowedPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return allowExtra[key]
}

// Environ returns a sanitized copy of os.Environ() with optional extra
// variables appended or overridden.
func Environ(extra ...string) []string {
	return Sanitize(os.Environ(), extra...)
}
