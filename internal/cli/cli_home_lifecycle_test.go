package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/e2edaemon"
)

const cliHomeOwnerFile = ".no-mistakes-cli-test-owner.json"
const cliTestPackage = "github.com/kunchenguid/no-mistakes/internal/cli"

// Start time is diagnostic only: even a reused PID protects its directory.
// Without a reliable liveness check, retention always fails toward keeping it.
type cliHomeOwner struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	Package   string    `json:"package"`
}

func sweepCLIHomeOrphans(base string, now time.Time) {
	entries, err := os.ReadDir(base)
	if err != nil {
		fmt.Fprintf(os.Stderr, "scan CLI test homes: %v\n", err)
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "nm-cli-home-") {
			continue
		}
		dir := filepath.Join(base, entry.Name())
		marker := filepath.Join(dir, cliHomeOwnerFile)
		info, err := os.Lstat(marker)
		reason := "owner exited"
		if os.IsNotExist(err) {
			info, err = entry.Info()
			if err != nil || now.Sub(info.ModTime()) <= 24*time.Hour {
				continue
			}
			reason = "unmarked home older than 24 hours"
		} else {
			// Malformed, unreadable, oversized or non-regular markers cannot
			// prove ownership. Never treat them as an unmarked legacy home.
			if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
				continue
			}
			data, err := os.ReadFile(marker)
			var owner cliHomeOwner
			if err != nil || json.Unmarshal(data, &owner) != nil || owner.PID <= 0 || owner.StartedAt.IsZero() || owner.Package != cliTestPackage {
				continue
			}
			alive, err := e2edaemon.ProcessAlive(owner.PID)
			if err != nil || alive {
				continue
			}
		}
		if err := removeTestDir(dir); err != nil {
			fmt.Fprintf(os.Stderr, "remove orphan CLI test home %s: %v\n", dir, err)
		} else {
			fmt.Fprintf(os.Stderr, "removed orphan CLI test home %s (%s)\n", dir, reason)
		}
	}
}

// runWithCLIHome owns the package-process scratch for every exit path we can
// handle. SIGKILL, runtime panics and go test timeouts bypass it; the next
// allocation removes their homes after checking the recorded owner's PID.
func runWithCLIHome(base string, run func(root, home string) int) int {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	sweepCLIHomeOrphans(base, time.Now())
	root, err := os.MkdirTemp(base, "nm-cli-test-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create test NM_HOME: %v\n", err)
		return 1
	}
	home, err := os.MkdirTemp(base, "nm-cli-home-")
	if err != nil {
		_ = os.RemoveAll(root)
		fmt.Fprintf(os.Stderr, "create test HOME: %v\n", err)
		return 1
	}
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			for _, dir := range []string{home, root} {
				if err := removeTestDir(dir); err != nil {
					fmt.Fprintf(os.Stderr, "remove CLI test scratch %s: %v\n", dir, err)
				}
			}
		})
	}
	defer cleanup()
	owner, err := json.Marshal(cliHomeOwner{PID: os.Getpid(), StartedAt: time.Now().UTC(), Package: cliTestPackage})
	if err == nil {
		err = os.WriteFile(filepath.Join(home, cliHomeOwnerFile), owner, 0o600)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "record CLI test HOME owner: %v\n", err)
		return 1
	}

	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case sig := <-signals:
			cleanup()
			code := 1
			if unixSignal, ok := sig.(syscall.Signal); ok {
				code = 128 + int(unixSignal)
			}
			os.Exit(code)
		case <-finished:
		}
	}()
	return run(root, home)
}

// Re-exec TestMain rather than starting a daemon or letting a test send a
// signal to its own process. The ready record is published after registration.
func runCLIHomeLifecycleHelper() int {
	return runWithCLIHome(os.Getenv("NM_CLI_HOME_TEST_BASE"), func(root, home string) int {
		data, _ := json.Marshal([]string{root, home})
		if err := os.WriteFile(os.Getenv("NM_CLI_HOME_TEST_READY"), data, 0o600); err != nil {
			return 1
		}
		switch os.Getenv("NM_CLI_HOME_LIFECYCLE_HELPER") {
		case "success":
			return 0
		case "failure":
			return 7
		default:
			for {
				time.Sleep(time.Hour)
			}
		}
	})
}

func startCLIHomeHelper(t *testing.T, mode string) (*exec.Cmd, string, string) {
	t.Helper()
	base := t.TempDir()
	ready := filepath.Join(t.TempDir(), "ready.json")
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "NM_TEST_START_DAEMON=0", "NM_FAKE_BIN=0", "NM_HOOK_HELPER=0",
		"NM_CLI_HOME_LIFECYCLE_HELPER="+mode, "NM_CLI_HOME_TEST_BASE="+base, "NM_CLI_HOME_TEST_READY="+ready)
	cmd.Stdout = &bytes.Buffer{}
	cmd.Stderr = &bytes.Buffer{}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd, base, ready
}

func readCLIHomeReady(t *testing.T, ready string) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(ready)
		var dirs []string
		if err == nil && json.Unmarshal(data, &dirs) == nil && len(dirs) == 2 {
			return dirs
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("CLI home helper did not become ready")
	return nil
}

func TestCLIHomeNormalExitCleansScratch(t *testing.T) {
	for _, tc := range []struct {
		mode string
		code int
	}{{"success", 0}, {"failure", 7}} {
		t.Run(tc.mode, func(t *testing.T) {
			cmd, base, ready := startCLIHomeHelper(t, tc.mode)
			if err := cmd.Wait(); cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != tc.code {
				t.Fatalf("exit = %v, want %d; stderr: %s", err, tc.code, cmd.Stderr)
			}
			readCLIHomeReady(t, ready)
			entries, err := os.ReadDir(base)
			if err != nil || len(entries) != 0 {
				t.Fatalf("normal %s left scratch: %v, %v", tc.mode, entries, err)
			}
		})
	}
}

func TestCLIHomeSignalCleansScratch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not support sending SIGINT/SIGTERM through os.Process.Signal")
	}
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			cmd, base, ready := startCLIHomeHelper(t, "wait")
			dirs := readCLIHomeReady(t, ready)
			data, err := os.ReadFile(filepath.Join(dirs[1], cliHomeOwnerFile))
			var owner cliHomeOwner
			if err != nil || json.Unmarshal(data, &owner) != nil || owner.PID != cmd.Process.Pid || owner.StartedAt.IsZero() || owner.Package != cliTestPackage {
				t.Fatalf("invalid owner marker: %s, %v", data, err)
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			exited := make(chan error, 1)
			go func() { exited <- cmd.Wait() }()
			select {
			case <-exited:
			case <-time.After(10 * time.Second):
				t.Fatal("signal did not terminate CLI home helper")
			}
			if code := cmd.ProcessState.ExitCode(); code != 128+int(sig) {
				t.Fatalf("signal exit = %d; stderr: %s", code, cmd.Stderr)
			}
			entries, err := os.ReadDir(base)
			if err != nil || len(entries) != 0 {
				t.Fatalf("signal left scratch: %v, %v", entries, err)
			}
		})
	}
}

func TestCLIHomeOrphanSweep(t *testing.T) {
	// An actual killed package-process home exercises the real liveness probe.
	cmd, base, ready := startCLIHomeHelper(t, "wait")
	dirs := readCLIHomeReady(t, ready)
	sweepCLIHomeOrphans(base, time.Now())
	if _, err := os.Stat(dirs[1]); err != nil {
		t.Fatalf("sweep deleted a live owner's home: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	now := time.Now().Truncate(time.Second)
	makeHome := func(name string, age time.Duration, marker []byte) string {
		t.Helper()
		dir := filepath.Join(base, name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if marker != nil {
			if err := os.WriteFile(filepath.Join(dir, cliHomeOwnerFile), marker, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chtimes(dir, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	liveMarker, _ := json.Marshal(cliHomeOwner{PID: os.Getpid(), StartedAt: now.Add(-48 * time.Hour), Package: cliTestPackage})
	live := makeHome("nm-cli-home-live", 48*time.Hour, liveMarker)
	old := makeHome("nm-cli-home-old", 25*time.Hour, nil)
	fresh := makeHome("nm-cli-home-fresh", time.Hour, nil)
	boundary := makeHome("nm-cli-home-boundary", 24*time.Hour, nil)
	malformed := makeHome("nm-cli-home-malformed", 48*time.Hour, []byte(`{"pid":0}`))
	unrelated := makeHome("unrelated", 48*time.Hour, nil)
	sweepCLIHomeOrphans(base, now)
	for _, dir := range []string{dirs[1], old} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("orphan survived: %s, %v", dir, err)
		}
	}
	for _, dir := range []string{live, fresh, boundary, malformed, unrelated} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("protected directory removed: %s, %v", dir, err)
		}
	}
}
