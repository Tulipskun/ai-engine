package sdk

import (
	"os"
	"path/filepath"
	"strings"
)

var projectRequirementFiles = []string{"product.md", "functional.md", "constraints.md", "decisions.md", "changes.md"}

func projectRequirements(workspace string) string {
	root := findRequirementsRoot(workspace)
	if root == "" {
		return ""
	}
	var b strings.Builder
	for _, name := range projectRequirementFiles {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || strings.TrimSpace(string(data)) == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("### ")
		b.WriteString(name)
		b.WriteByte('\n')
		b.WriteString(strings.TrimSpace(string(data)))
	}
	return b.String()
}

func findRequirementsRoot(workspace string) string {
	path, err := filepath.Abs(strings.TrimSpace(workspace))
	if err != nil || path == "." {
		return ""
	}
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		path = filepath.Dir(path)
	}
	for {
		candidate := filepath.Join(path, "requirements")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(path)
		if parent == path {
			return ""
		}
		path = parent
	}
}
