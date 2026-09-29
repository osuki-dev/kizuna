package ignore

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// Rule represents a parsed ignore pattern
type Rule struct {
	Pattern   string
	IsNegated bool
	DirOnly   bool
	RootOnly  bool
}

// Matcher evaluates files against .gitignore and default ignore rules
type Matcher struct {
	rootDir string
	rules   []Rule
}

// DefaultIgnorePatterns contains universally ignored files and directories
var DefaultIgnorePatterns = []string{
	".git/",
	"node_modules/",
	".DS_Store",
	"*.log",
	"*.swp",
	"*.tmp",
	".kizuna/",
}

// NewMatcher loads .gitignore from rootDir and attaches default rules
func NewMatcher(rootDir string) *Matcher {
	m := &Matcher{
		rootDir: rootDir,
		rules:   make([]Rule, 0),
	}

	// 1. Add default rules
	for _, p := range DefaultIgnorePatterns {
		m.addRule(p)
	}

	// 2. Read .gitignore if present in rootDir
	gitignorePath := filepath.Join(rootDir, ".gitignore")
	if f, err := os.Open(gitignorePath); err == nil {
		defer func() { _ = f.Close() }()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			m.addRule(line)
		}
	}

	return m
}

func (m *Matcher) addRule(line string) {
	rule := Rule{}

	if strings.HasPrefix(line, "!") {
		rule.IsNegated = true
		line = strings.TrimPrefix(line, "!")
	}

	if strings.HasSuffix(line, "/") {
		rule.DirOnly = true
		line = strings.TrimSuffix(line, "/")
	}

	if strings.HasPrefix(line, "/") {
		rule.RootOnly = true
		line = strings.TrimPrefix(line, "/")
	}

	rule.Pattern = filepath.ToSlash(line)
	m.rules = append(m.rules, rule)
}

// ShouldIgnore returns true if the relative path should be excluded
func (m *Matcher) ShouldIgnore(relPath string, isDir bool) bool {
	// Normalize path to slash
	cleanPath := filepath.ToSlash(filepath.Clean(relPath))
	if cleanPath == "." || cleanPath == "" {
		return false
	}

	baseName := filepath.Base(cleanPath)

	// Hardcoded safety net: always ignore .git and node_modules
	if baseName == ".git" || baseName == "node_modules" {
		return true
	}
	if strings.HasPrefix(cleanPath, ".git/") || strings.Contains(cleanPath, "/.git/") {
		return true
	}
	if strings.HasPrefix(cleanPath, "node_modules/") || strings.Contains(cleanPath, "/node_modules/") {
		return true
	}

	ignored := false

	// Check if any parent directory is ignored by a DirOnly rule
	parts := strings.Split(cleanPath, "/")

	for _, rule := range m.rules {
		matched := false

		if rule.DirOnly {
			// If target is a directory and matches
			if isDir && m.matchPattern(rule, cleanPath, baseName) {
				matched = true
			} else {
				// Check each ancestor directory prefix
				curPrefix := ""
				for i := 0; i < len(parts)-1; i++ {
					if curPrefix == "" {
						curPrefix = parts[i]
					} else {
						curPrefix = curPrefix + "/" + parts[i]
					}
					if m.matchPattern(rule, curPrefix, parts[i]) {
						matched = true
						break
					}
				}
			}
		} else {
			if m.matchPattern(rule, cleanPath, baseName) {
				matched = true
			}
		}

		if matched {
			if rule.IsNegated {
				ignored = false
			} else {
				ignored = true
			}
		}
	}

	return ignored
}

func (m *Matcher) matchPattern(rule Rule, targetPath, baseName string) bool {
	if rule.RootOnly {
		if ok, _ := filepath.Match(rule.Pattern, targetPath); ok {
			return true
		}
		if strings.HasPrefix(targetPath, rule.Pattern+"/") {
			return true
		}
		return false
	}

	if strings.Contains(rule.Pattern, "/") {
		if ok, _ := filepath.Match(rule.Pattern, targetPath); ok {
			return true
		}
		if strings.HasPrefix(targetPath, rule.Pattern+"/") {
			return true
		}
		return false
	}

	// No slash: matches any segment or filename
	if ok, _ := filepath.Match(rule.Pattern, baseName); ok {
		return true
	}
	if ok, _ := filepath.Match(rule.Pattern, targetPath); ok {
		return true
	}

	return false
}
