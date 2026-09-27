// This module owns the workspace path discipline every surviving tool relies on:
// safePath resolves a model-supplied path inside the workspace root, rejecting
// traversal and symlink escapes (CHANGE-087). Only the read tool is registered;
// bash reaches the rest of the workspace through the shell.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const maxFileBytes = 4 << 20

func filepathAbsClean(path string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("tools: workspace is not a directory")
	}
	return abs, nil
}

func safePath(root, name string) (string, error) {
	if name == "" {
		name = "."
	}
	candidate := name
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(root, candidate)
	}
	abs, err := filepath.Abs(filepath.Clean(candidate))
	if err != nil {
		return "", err
	}
	root, err = filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "", err
	}
	if !withinRoot(root, abs) {
		return "", fmt.Errorf("path escapes workspace: %q", name)
	}
	resolved, err := resolveExistingPath(abs)
	if err != nil {
		return "", err
	}
	if !withinRoot(root, resolved) {
		return "", fmt.Errorf("path escapes workspace through symlink: %q", name)
	}
	return abs, nil
}

func withinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func resolveExistingPath(path string) (string, error) {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Abs(resolved)
	}
	current := path
	var missing []string
	for {
		if _, err := os.Lstat(current); err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			resolved, err = filepath.Abs(resolved)
			if err != nil {
				return "", err
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("cannot resolve path: %q", path)
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

type readFileArgs struct {
	Path string `json:"path"`
}

func readFileTool(rootAt func(context.Context) string) handler {
	return func(ctx context.Context, raw json.RawMessage) (string, error) {
		root := rootAt(ctx)
		var args readFileArgs
		if err := json.Unmarshal(raw, &args); err != nil {
			return "", err
		}
		path, err := safePath(root, args.Path)
		if err != nil {
			return "", err
		}
		info, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		if info.IsDir() {
			return "", errors.New("path is a directory")
		}
		if info.Size() > maxFileBytes {
			return "", fmt.Errorf("file exceeds %d byte limit", maxFileBytes)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
}
