package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// Runtime is repository-only and trusted-only, even with allow_repo_commands.
// NodePath is an optional absolute path to an operator-installed node binary.
type Runtime struct {
	Node     string `yaml:"node"`
	NodePath string `yaml:"node_path"`
}

var nodeVersionPin = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func (r Runtime) Validate() error {
	if r.Node != "" && (len(r.Node) > 64 || !nodeVersionPin.MatchString(r.Node)) {
		return fmt.Errorf("runtime.node must be an exact Node version such as \"22.23.1\"")
	}
	if r.NodePath != "" && (r.Node == "" || !filepath.IsAbs(r.NodePath)) {
		return fmt.Errorf("runtime.node_path requires runtime.node and an absolute path to the node executable")
	}
	if r.NodePath != "" {
		name := filepath.Base(r.NodePath)
		expected := "node"
		if runtime.GOOS == "windows" {
			expected = "node.exe"
			name = strings.ToLower(name)
		}
		if name != expected || strings.ContainsAny(r.NodePath, "\x00\r\n") || strings.ContainsRune(filepath.Dir(r.NodePath), os.PathListSeparator) {
			return fmt.Errorf("runtime.node_path must name node or node.exe in a directory usable on PATH")
		}
	}
	return nil
}
