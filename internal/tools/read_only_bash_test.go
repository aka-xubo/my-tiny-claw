package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aka-xubo/my-tiny-claw/internal/schema"
)

func requireReadOnlySandbox(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "linux" {
		if _, err := os.Stat("/usr/bin/bwrap"); err != nil {
			t.Skip("bubblewrap not installed; tool fails closed on this host")
		}
	} else if runtime.GOOS != "darwin" {
		t.Skip("read-only bash is unsupported and fails closed on this host")
	}
}

func TestReadOnlyBashSearchesAndPreventsWrites(t *testing.T) {
	requireReadOnlySandbox(t)
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(filepath.Join(dir, "input.txt"), []byte("confirm \na|b\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Exercise git's real --output behavior inside a repository.
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %s %v", out, err)
	}
	tool := NewReadOnlyBashTool(dir)
	for _, command := range []string{`grep -rn "confirm " .`, `grep 'a\|b' input.txt`, `cat input.txt | head -n 1`, `git --version`, `git status --short`, `git diff --stat`} {
		args, _ := json.Marshal(bashArgs{Command: command})
		if out, err := tool.Execute(context.Background(), args); err != nil {
			t.Fatalf("read query failed: %s: %q, %v", command, out, err)
		}
	}
	for _, command := range []string{
		`printf x > out.txt`,
		`sed -n -i 's/a/b/p' input.txt`,
		`sed -n '1w out.txt' input.txt`,
		`find . -fprint out.txt`,
		`git diff --output=out.txt`,
		`bash -c 'printf x > out.txt'`,
		`printf x > "` + outside + `"`,
		`rm input.txt`,
		`ln -s input.txt out.txt`,
		`mkdir out.txt`,
	} {
		args, _ := json.Marshal(bashArgs{Command: command})
		// Do not rely on the tool's error: assert the filesystem boundary itself.
		_, _ = tool.Execute(context.Background(), args)
		if _, err := os.Lstat(filepath.Join(dir, "out.txt")); !os.IsNotExist(err) {
			t.Fatalf("sandbox permitted write: %s (%v)", command, err)
		}
		if _, err := os.Stat(outside); !os.IsNotExist(err) {
			t.Fatalf("sandbox permitted outside write: %s (%v)", command, err)
		}
		data, err := os.ReadFile(filepath.Join(dir, "input.txt"))
		if err != nil || string(data) != "confirm \na|b\n" {
			t.Fatalf("sandbox modified input: %s: %q %v", command, data, err)
		}
	}
}

func TestReadOnlyBashPreprocessorCannotEscapeSandbox(t *testing.T) {
	requireReadOnlySandbox(t)
	dir := t.TempDir()
	// A script is allowed to execute, but it inherits the read-only sandbox.
	if err := os.WriteFile(filepath.Join(dir, "evil.sh"), []byte("#!/bin/bash\nprintf x > out.txt\ncat\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "input.txt"), []byte("foo"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not installed")
	}
	args, _ := json.Marshal(bashArgs{Command: `rg --pre ./evil.sh foo input.txt`})
	out, _ := NewReadOnlyBashTool(dir).Execute(context.Background(), args)
	if !strings.Contains(out, "foo") {
		t.Fatalf("preprocessor did not actually run: %q", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.txt")); !os.IsNotExist(err) {
		t.Fatalf("preprocessor escaped sandbox: %v", err)
	}
}

func TestReadOnlyBashProtocol(t *testing.T) {
	tool := NewReadOnlyBashTool(t.TempDir())
	middleware := NewReadOnlyBashMiddleware()
	for _, args := range []string{`{"action":"list"}`, `{"session_id":"x","command":"ls"}`} {
		if _, err := tool.Execute(context.Background(), json.RawMessage(args)); err == nil {
			t.Fatalf("one-shot tool accepted sessions: %s", args)
		}
		if ok, _ := middleware(context.Background(), schema.ToolCall{Name: "bash", Arguments: json.RawMessage(args)}); ok {
			t.Fatalf("middleware accepted sessions: %s", args)
		}
	}
}

// The helper runs in a separate test process under the actual OS sandbox.
func TestReadOnlyNetworkHelper(t *testing.T) {
	network := os.Getenv("CLAW_TEST_READONLY_NETWORK")
	if network == "" {
		return
	}
	conn, err := net.DialTimeout(network, os.Getenv("CLAW_TEST_READONLY_ADDRESS"), time.Second)
	if err == nil {
		conn.Close()
		os.Exit(17)
	}
	fmt.Println("NETWORK_DENIED")
}

func TestReadOnlyBashDeniesTCPAndHostUnixSockets(t *testing.T) {
	requireReadOnlySandbox(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, network := range []string{"unix", "tcp"} {
		t.Run(network, func(t *testing.T) {
			address := "127.0.0.1:0"
			if network == "unix" {
				address = filepath.Join(t.TempDir(), "test.sock")
			}
			listener, err := net.Listen(network, address)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			t.Setenv("CLAW_TEST_READONLY_NETWORK", network)
			t.Setenv("CLAW_TEST_READONLY_ADDRESS", listener.Addr().String())
			command := "'" + strings.ReplaceAll(binary, "'", "'\"'\"'") + "' -test.run=^TestReadOnlyNetworkHelper$"
			args, _ := json.Marshal(bashArgs{Command: command})
			out, err := NewReadOnlyBashTool(t.TempDir()).Execute(context.Background(), args)
			if err != nil || !strings.Contains(out, "NETWORK_DENIED") {
				t.Fatalf("sandbox did not deny %s connection: %q %v", network, out, err)
			}
		})
	}
}
