// Package classify implements an offline, deterministic task-risk classifier.
// It reads only path metadata in the supplied repository, never file contents.
package classify

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"dispatch/internal/adapter"
)

// Bump featureSchema whenever names, order, extraction or normalization change.
const featureSchema = 1
const FeatureSchema = featureSchema
const featureCount = 24

type Features [featureCount]float64

// The divisors below are fixed saturation limits, not corpus statistics.
var names = [featureCount]string{
	"bias", "prompt_chars", "prompt_words", "prompt_lines", "list_items", "code_fences",
	"existing_files", "existing_packages", "create", "refactor", "fix", "investigate", "test",
	"risk_auth", "risk_security", "risk_money", "risk_data", "risk_migration", "verification_command",
	"readonly", "write", "tier_easy", "tier_normal", "tier_hard",
}

// FeatureNames returns the feature names in weight order, for display.
func FeatureNames() []string { return append([]string(nil), names[:]...) }

type Input struct {
	Prompt   string
	Dir      string
	ReadOnly bool
	Tier     adapter.Tier
}

var tokens = regexp.MustCompile(`[\pL\pN_./-]+`)
var verificationCommand = regexp.MustCompile(`(?m)\b(go (test|vet|build)|gofmt|(?:pnpm|npm|yarn|bun) (?:run )?(test|lint|build|typecheck)|pytest|cargo (test|check)|make (test|check)|swift test|xcodebuild)\b`)
var accents = strings.NewReplacer("á", "a", "ã", "a", "â", "a", "à", "a", "é", "e", "ê", "e", "í", "i", "ó", "o", "ô", "o", "õ", "o", "ú", "u", "ç", "c")

func Extract(in Input) Features {
	p := accents.Replace(strings.ToLower(in.Prompt))
	words := tokens.FindAllString(p, -1)
	f := Features{1, norm(utf8.RuneCountInString(in.Prompt), 8000), norm(len(words), 1500), norm(strings.Count(p, "\n")+1, 100)}
	items := 0
	for _, line := range strings.Split(p, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") || regexpListItem(line) {
			items++
		}
	}
	f[4], f[5] = norm(items, 20), norm(strings.Count(p, "```"), 10)
	files, packages := references(in.Dir, in.Prompt)
	f[6], f[7] = norm(files, 20), norm(packages, 10)
	stems := [][]string{
		{"creat", "add", "cria", "adicion", "implement"},
		{"refactor", "refator", "reestrutur"},
		{"fix", "corrig", "consert", "repair"},
		{"investig", "analis", "analy", "inspect", "revis", "review"},
		{"test", "valid", "verific"},
		{"auth", "autentic", "autoriz", "permiss", "oauth", "login"},
		{"security", "secur", "seguran", "credential", "credencia", "crypt", "criptograf"},
		{"money", "dinheiro", "payment", "pagament", "billing", "finance", "cobranc"},
		{"data", "dados", "database", "banco", "persist", "storage"},
		{"migrat", "migrac", "migrar", "schema"},
	}
	for i, choices := range stems {
		for _, word := range words {
			// Match path components too (e.g. internal/auth.go).
			for _, part := range strings.FieldsFunc(word, func(r rune) bool { return r == '/' || r == '.' || r == '_' || r == '-' }) {
				for _, stem := range choices {
					if strings.HasPrefix(part, stem) {
						f[8+i] = 1
					}
				}
			}
		}
	}
	if verificationCommand.MatchString(p) {
		f[18] = 1
	}
	if in.ReadOnly {
		f[19] = 1
	} else {
		f[20] = 1
	}
	return f.WithTier(in.Tier)
}

// WithTier replaces only tier covariates, preserving the pre-execution snapshot.
func (f Features) WithTier(t adapter.Tier) Features {
	f[21], f[22], f[23] = 0, 0, 0
	switch t {
	case adapter.TierEasy:
		f[21] = 1
	case adapter.TierNormal:
		f[22] = 1
	case adapter.TierHard:
		f[23] = 1
	}
	return f
}

func regexpListItem(s string) bool {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return i > 0 && i+1 < len(s) && (s[i] == '.' || s[i] == ')') && s[i+1] == ' '
}

func norm(n, limit int) float64 { return clamp(float64(n)/float64(limit), 0, 1) }

// references checks at most 256 distinct relative paths, with no traversal,
// hidden components or symlinks. Directories represent cited local packages.
// No repository scan, subprocess, config or credential read is needed.
func references(dir, prompt string) (files, packages int) {
	if dir == "" {
		dir = "."
	}
	seen := map[string]bool{}
	for _, word := range tokens.FindAllString(prompt, -1) {
		p := filepath.Clean(strings.TrimSuffix(word, "."))
		if p == "." || filepath.IsAbs(p) || len(p) > 256 || seen[p] {
			continue
		}
		seen[p] = true
		if len(seen) > 256 {
			break
		}
		path := dir
		var info os.FileInfo
		for _, part := range strings.Split(p, string(filepath.Separator)) {
			if strings.HasPrefix(part, ".") {
				info = nil
				break
			}
			path = filepath.Join(path, part)
			var err error
			info, err = os.Lstat(path)
			if err != nil || info.Mode()&os.ModeSymlink != 0 {
				info = nil
				break
			}
		}
		if info != nil {
			if info.IsDir() {
				packages++
			} else if info.Mode().IsRegular() {
				files++
			}
		}
	}
	return
}

// Sensitive is the fixed safety rule, independent of learned weights.
func (f Features) Sensitive() bool {
	for _, x := range f[13:18] {
		if x > 0 {
			return true
		}
	}
	return false
}

func Higher(t adapter.Tier) adapter.Tier {
	switch t {
	case adapter.TierEasy:
		return adapter.TierNormal
	case adapter.TierNormal:
		return adapter.TierHard
	default:
		return t
	}
}
