package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// Selection is scoped to this Execute call, including agent tools and prepare,
// test, lint and format commands. Never switch the daemon's ambient runtime or
// ask a manager to install a version or change its default.
func prepareRepositoryRuntime(sctx *pipeline.StepContext) (func(), error) {
	restore := func() {}
	pin, source, err := repositoryNodePin(sctx.WorkDir, sctx.Config.Runtime)
	if err != nil {
		return restore, fmt.Errorf("select repository Node runtime: %w", err)
	}
	if pin == "" {
		return restore, nil
	}
	match, err := compileNodeRange(pin)
	if err != nil {
		return restore, fmt.Errorf("Node pin %q from %s: %w; set runtime.node to an exact installed version", pin, source, err)
	}
	env := stepEnvironment(sctx)
	if env == nil {
		env = os.Environ()
	}
	ctx, cancel := context.WithTimeout(sctx.Ctx, 15*time.Second)
	defer cancel()
	node, version, err := resolveInstalledNode(ctx, env, sctx.Config.Runtime.NodePath, match)
	if err != nil {
		return restore, fmt.Errorf("Node %q pinned by %s is unavailable: %w; install the pinned version with nvm, fnm or Volta, or set runtime.node_path to its executable (host Node is not used as a fallback)", pin, source, err)
	}
	original := sctx.Env
	sctx.Env = prependRuntimePath(env, filepath.Dir(node))
	if sctx.Log != nil {
		sctx.Log(fmt.Sprintf("Runtime: Node %s (%s: %s), executable %s", version, source, pin, node))
	}
	return func() { sctx.Env = original }, nil
}

func repositoryNodePin(dir string, cfg config.Runtime) (string, string, error) {
	if err := cfg.Validate(); err != nil {
		return "", "", err
	}
	if cfg.Node != "" {
		return cfg.Node, "runtime.node", nil
	}
	for _, name := range []string{".node-version", ".nvmrc", "package.json"} {
		data, err := readRuntimePinFile(filepath.Join(dir, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", "", fmt.Errorf("read %s: %w", name, err)
		}
		pin := strings.TrimSpace(string(data))
		if name == "package.json" {
			var pkg struct {
				Engines struct {
					Node string `json:"node"`
				} `json:"engines"`
			}
			if err := json.Unmarshal(data, &pkg); err != nil {
				return "", "", fmt.Errorf("read package.json engines.node: %w", err)
			}
			pin = strings.TrimSpace(pkg.Engines.Node)
			if pin == "" {
				return "", "", nil
			}
			name = "package.json engines.node"
		}
		if pin == "" {
			return "", "", fmt.Errorf("%s contains an empty Node pin", name)
		}
		if len(pin) > 512 {
			return "", "", fmt.Errorf("%s Node pin exceeds 512 bytes", name)
		}
		return pin, name, nil
	}
	return "", "", nil
}

func readRuntimePinFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	const limit = 1 << 20
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if len(data) > limit {
		return nil, fmt.Errorf("pin file exceeds %d bytes", limit)
	}
	return data, err
}

func prependRuntimePath(env []string, bin string) []string {
	path, _ := envValue(env, "PATH")
	if path != "" {
		bin += string(os.PathListSeparator) + path
	}
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if envKey(entry) != envKey("PATH=") {
			out = append(out, entry)
		}
	}
	return append(out, "PATH="+bin)
}

type installedNode struct {
	path    string
	version nodeVersion
}

func resolveInstalledNode(ctx context.Context, env []string, configured string, match func(nodeVersion) bool) (string, string, error) {
	paths := []string{configured}
	if configured == "" {
		paths = installedNodePaths(env, match)
	}
	for _, path := range paths {
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}
		info, err := os.Stat(path)
		if err != nil || !pathCandidateUsable(runtime.GOOS, path, info) {
			if configured != "" {
				return "", "", fmt.Errorf("%s is not an executable", path)
			}
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		cmd := exec.CommandContext(probeCtx, path, "--version")
		cmd.Env = env
		shellenv.ConfigureShellCommand(cmd)
		output, err := shellenv.OutputShellCommand(cmd)
		cancel()
		version, count, parseErr := parseNodeVersion(strings.TrimSpace(string(output)))
		if err == nil && parseErr == nil && count == 3 && match(version) {
			return path, version.String(), nil
		}
		if configured != "" {
			return "", "", fmt.Errorf("%s did not report the requested version (reported %q, error %v)", path, strings.TrimSpace(string(output)), err)
		}
	}
	return "", "", fmt.Errorf("no matching installed node executable found")
}

func installedNodePaths(env []string, match func(nodeVersion) bool) []string {
	value := func(key, fallback string) string {
		if v, ok := envValue(env, key); ok {
			return v
		}
		return fallback
	}
	home := value("HOME", "")
	if runtime.GOOS == "windows" {
		home = value("USERPROFILE", home)
	}
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	data := value("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	nvm := value("NVM_DIR", filepath.Join(home, ".nvm"))
	fnm := value("FNM_DIR", filepath.Join(data, "fnm"))
	volta := value("VOLTA_HOME", filepath.Join(home, ".volta"))
	roots := []struct{ root, suffix string }{
		{filepath.Join(nvm, "versions", "node"), "bin"},
		{filepath.Join(fnm, "node-versions"), filepath.Join("installation", "bin")},
		{filepath.Join(volta, "tools", "image", "node"), "bin"},
	}
	if runtime.GOOS == "darwin" {
		roots = append(roots, struct{ root, suffix string }{filepath.Join(home, "Library", "Application Support", "fnm", "node-versions"), filepath.Join("installation", "bin")})
	}
	name := "node"
	if runtime.GOOS == "windows" {
		name = "node.exe"
		roots = []struct{ root, suffix string }{
			{value("NVM_HOME", filepath.Join(value("APPDATA", ""), "nvm")), ""},
			{filepath.Join(value("FNM_DIR", filepath.Join(value("LOCALAPPDATA", ""), "fnm")), "node-versions"), "installation"},
			{filepath.Join(volta, "tools", "image", "node"), ""},
		}
	}
	var installed []installedNode
	for _, root := range roots {
		if !filepath.IsAbs(root.root) {
			continue
		}
		entries, _ := os.ReadDir(root.root)
		for _, entry := range entries {
			v, count, err := parseNodeVersion(entry.Name())
			if err == nil && count == 3 && match(v) {
				installed = append(installed, installedNode{filepath.Join(root.root, entry.Name(), root.suffix, name), v})
			}
		}
	}
	// Ranges choose the newest installed manager version, never download one.
	slices.SortStableFunc(installed, func(a, b installedNode) int { return -a.version.Compare(b.version) })
	paths := make([]string, 0, len(installed))
	for _, node := range installed {
		paths = append(paths, node.path)
	}
	// A directly installed or custom PATH runtime is acceptable only after its
	// actual --version matches. Relative PATH entries are never runtime sources.
	path, _ := envValue(env, "PATH")
	for _, dir := range filepath.SplitList(path) {
		if filepath.IsAbs(dir) {
			paths = append(paths, filepath.Join(dir, name))
		}
	}
	return paths
}
