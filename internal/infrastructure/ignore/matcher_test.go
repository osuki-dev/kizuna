package ignore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMatcherDefaults(t *testing.T) {
	tempDir := t.TempDir()
	matcher := NewMatcher(tempDir)

	tests := []struct {
		path     string
		isDir    bool
		expected bool
	}{
		{"node_modules", true, true},
		{"node_modules/express/index.js", false, true},
		{".git", true, true},
		{".git/config", false, true},
		{".DS_Store", false, true},
		{"src/App.tsx", false, false},
		{"package.json", false, false},
		{"test.log", false, true},
	}

	for _, tt := range tests {
		got := matcher.ShouldIgnore(tt.path, tt.isDir)
		if got != tt.expected {
			t.Errorf("ShouldIgnore(%q, %v) = %v; expected %v", tt.path, tt.isDir, got, tt.expected)
		}
	}
}

func TestMatcherWithGitignore(t *testing.T) {
	tempDir := t.TempDir()
	gitignoreContent := `
# Comments
dist/
*.secret
/build
!build/keep.txt
`
	if err := os.WriteFile(filepath.Join(tempDir, ".gitignore"), []byte(gitignoreContent), 0644); err != nil {
		t.Fatalf("failed to write .gitignore: %v", err)
	}

	matcher := NewMatcher(tempDir)

	tests := []struct {
		path     string
		isDir    bool
		expected bool
	}{
		{"dist", true, true},
		{"dist/bundle.js", false, true},
		{"sub/my.secret", false, true},
		{"build", true, true},
		{"src/build", true, false}, // /build only matches root
		{"build/keep.txt", false, false}, // negated rule
		{"src/index.ts", false, false},
	}

	for _, tt := range tests {
		got := matcher.ShouldIgnore(tt.path, tt.isDir)
		if got != tt.expected {
			t.Errorf("ShouldIgnore(%q, %v) = %v; expected %v", tt.path, tt.isDir, got, tt.expected)
		}
	}
}
