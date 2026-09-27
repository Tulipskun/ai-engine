package main

import (
	"os"
	"path/filepath"
	"strings"
)

var requirementFiles = []string{"product.md", "functional.md", "constraints.md", "decisions.md", "changes.md"}

func projectRequirements(workspace string) string {
	root := findRequirementsRoot(workspace)
	if root == "" {
		return ""
	}
	var b strings.Builder
	for _, name := range requirementFiles {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || len(strings.TrimSpace(string(data))) == 0 {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("### ")
		b.WriteString(name)
		b.WriteString("\n")
		b.WriteString(strings.TrimSpace(string(data)))
	}
	return b.String()
}

func findRequirementsRoot(workspace string) string {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return ""
	}
	path, err := filepath.Abs(workspace)
	if err != nil {
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
