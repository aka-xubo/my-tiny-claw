//go:build darwin

package sandbox_test

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aka-xubo/my-tiny-claw/internal/fshelper"
	sb "github.com/aka-xubo/my-tiny-claw/internal/sandbox"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "__fs-helper" {
		if err := fshelper.Serve(os.Stdin, os.Stdout); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) > 3 && os.Args[1] == "__net-probe" {
		c, err := net.DialTimeout(os.Args[2], os.Args[3], time.Second)
		if err == nil {
			_, err = c.Write([]byte("probe"))
			c.Close()
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	os.Exit(m.Run())
}
func requireSeatbelt(t *testing.T) {
	t.Helper()
	out, err := exec.Command("/usr/bin/sandbox-exec", "-p", "(version 1) (allow default)", "/usr/bin/true").CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "sandbox_apply: Operation not permitted") && os.Getenv("CLAW_REQUIRE_SEATBELT") != "1" {
			t.Skip("Seatbelt unavailable in outer sandbox")
		}
		t.Fatalf("Seatbelt probe: %v %s", err, out)
	}
}
func quoted(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
func runCommand(t *testing.T, p *sb.Profile, dir, command string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd, cleanup, err := sb.Prepare(ctx, sb.SpawnRequest{Argv: []string{"/bin/bash", "-c", command}, Dir: dir, Profile: p})
	if err != nil {
		return "", err
	}
	defer cleanup()
	out, err := cmd.CombinedOutput()
	return string(out), err
}
func fixture(t *testing.T) (string, string, *sb.Profile) {
	t.Helper()
	requireSeatbelt(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(root, "ws")
	outside := filepath.Join(root, "outside")
	for _, dir := range []string{ws, outside, filepath.Join(ws, ".git", "hooks")} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	p := &sb.Profile{Name: "test", Files: []sb.FileRule{{Path: "/", Access: sb.AccessRead}, {Path: ws, Access: sb.AccessWrite}, {Path: filepath.Join(ws, ".git"), Access: sb.AccessRead}}, ProtectedRoots: []string{ws}}
	return ws, outside, p
}
func TestSeatbeltAttackMatrix(t *testing.T) {
	for _, backend := range []string{"bash", "helper"} {
		t.Run(backend, func(t *testing.T) {
			ws, outside, p := fixture(t)
			secret := filepath.Join(outside, "secret")
			if err := os.WriteFile(secret, []byte("original"), 0644); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(ws, "symlink")
			if err := os.Symlink(secret, link); err != nil {
				t.Fatal(err)
			}
			denied := filepath.Join(ws, "secret.env")
			if err := os.WriteFile(denied, []byte("original"), 0644); err != nil {
				t.Fatal(err)
			}
			p.Files = append(p.Files, sb.FileRule{Path: secret, Access: sb.AccessDeny}, sb.FileRule{Path: ws, Glob: "**/*.env", Access: sb.AccessDeny})
			session, err := sb.NewSession(p, sb.ApprovalNever, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"outside", "protected", "protected-case", "symlink", "glob", "allowed"} {
				t.Run(name, func(t *testing.T) {
					target := map[string]string{"outside": secret, "protected": filepath.Join(ws, ".git", "hooks", "x"), "protected-case": filepath.Join(ws, ".GIT", "hooks", "x"), "symlink": link, "glob": denied, "allowed": filepath.Join(ws, "ok")}[name]
					allowed := name == "allowed"
					if backend == "bash" {
						out, err := runCommand(t, p, ws, "printf changed > "+quoted(target))
						if (err == nil) != allowed {
							t.Fatalf("allowed=%v error=%v output=%s", allowed, err, out)
						}
					} else {
						ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer cancel()
						response, err := fshelper.Run(ctx, session, executable, ws, p, fshelper.Request{Op: "write", Path: target, Content: "changed"})
						if err != nil {
							t.Fatal(err)
						}
						if response.OK != allowed {
							t.Fatalf("allowed=%v response=%+v", allowed, response)
						}
					}
					if !allowed {
						if data, err := os.ReadFile(secret); err != nil || string(data) != "original" {
							t.Fatalf("outside changed: %q %v", data, err)
						}
					}
				})
			}
			for _, target := range []string{secret, denied} {
				t.Run("read_"+filepath.Base(target), func(t *testing.T) {
					if backend == "bash" {
						if out, err := runCommand(t, p, ws, "cat "+quoted(target)); err == nil {
							t.Fatalf("read allowed: %s", out)
						}
					} else {
						response, err := fshelper.Run(context.Background(), session, executable, ws, p, fshelper.Request{Op: "read", Path: target, MaxBytes: 100})
						if err != nil {
							t.Fatal(err)
						}
						if response.OK {
							t.Fatal("read allowed")
						}
					}
				})
			}
		})
	}
}
func TestSeatbeltApprovedPermissionsAndGlobPriority(t *testing.T) {
	ws, _, p := fixture(t)
	target := filepath.Join(ws, ".git", "test.env")
	p.Files = append(p.Files, sb.FileRule{Path: ws, Glob: "**/*.env", Access: sb.AccessDeny}, sb.FileRule{Path: filepath.Join(ws, ".git"), Access: sb.AccessWrite, Approved: true})
	if out, err := runCommand(t, p, ws, "printf approved > "+quoted(target)); err != nil {
		t.Fatalf("approved write failed: %v %s", err, out)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "approved" {
		t.Fatalf("content=%s err=%v", data, err)
	}
	// A more specific literal write cannot override a glob deny without approval.
	p.Files[len(p.Files)-1].Approved = false
	if out, err := runCommand(t, p, ws, "printf changed > "+quoted(target)); err == nil {
		t.Fatalf("glob bypass: %s", out)
	}
}
func TestSeatbeltRenameAndCaseProtection(t *testing.T) {
	ws, _, p := fixture(t)
	for _, command := range []string{"mv .git moved", "mv " + quoted(ws) + " " + quoted(ws+"-moved"), "printf x > .GIT/hooks/x", "rm -rf " + quoted(ws)} {
		if out, err := runCommand(t, p, ws, command); err == nil {
			t.Fatalf("protection bypass %q: %s", command, out)
		}
	}
}
func TestSeatbeltHelperFileOnlyWrite(t *testing.T) {
	ws, _, p := fixture(t)
	target := filepath.Join(ws, "file")
	if err := os.WriteFile(target, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	p.Files = []sb.FileRule{{Path: "/", Access: sb.AccessRead}, {Path: target, Access: sb.AccessWrite}}
	session, _ := sb.NewSession(p, sb.ApprovalNever, nil, nil)
	defer session.Close()
	executable, _ := os.Executable()
	response, err := fshelper.Run(context.Background(), session, executable, ws, p, fshelper.Request{Op: "write", Path: target, Content: "after"})
	if err != nil || !response.OK {
		t.Fatalf("file-only fallback: %+v %v", response, err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "after" {
		t.Fatalf("content=%q %v", data, err)
	}
}
func TestSeatbeltEnvironmentAndDescendant(t *testing.T) {
	ws, outside, p := fixture(t)
	t.Setenv("CODEX_API_KEY", "should-not-leak")
	if out, err := runCommand(t, p, ws, "env"); err != nil || strings.Contains(out, "should-not-leak") {
		t.Fatalf("environment leaked: %v", err)
	}
	target := filepath.Join(outside, "delayed")
	command := fmt.Sprintf("/bin/bash -c %s & wait", quoted("sleep 0.05; printf x > "+quoted(target)))
	_, _ = runCommand(t, p, ws, command)
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("descendant file exists: %v", err)
	}
}
func TestSeatbeltHardlinkBehavior(t *testing.T) {
	ws, outside, p := fixture(t)
	target := filepath.Join(outside, "hard-original")
	alias := filepath.Join(ws, "alias")
	if err := os.WriteFile(target, []byte("original"), 0644); err != nil {
		t.Fatal(err)
	}
	// Verify sandboxed creation of a link, and an existing hardlink separately.
	out, err := runCommand(t, p, ws, "ln "+quoted(target)+" "+quoted(alias))
	t.Logf("sandbox link creation: error=%v output=%s", err, out)
	if err == nil {
		t.Fatal("sandbox permitted creation of an outside hardlink")
	}
	if _, err := os.Stat(alias); os.IsNotExist(err) {
		if err := os.Link(target, alias); err != nil {
			t.Fatal(err)
		}
	}
	out, err = runCommand(t, p, ws, "printf changed > "+quoted(alias))
	data, _ := os.ReadFile(target)
	t.Logf("existing hardlink in-place write: error=%v changed=%v output=%s", err, string(data) != "original", out)
	// The helper's atomic replace must not write through an existing hardlink.
	_ = os.WriteFile(target, []byte("original"), 0644)
	executable, _ := os.Executable()
	session, _ := sb.NewSession(p, sb.ApprovalNever, nil, nil)
	defer session.Close()
	response, err := fshelper.Run(context.Background(), session, executable, ws, p, fshelper.Request{Op: "write", Path: alias, Content: "helper"})
	if err != nil || !response.OK {
		t.Fatalf("helper atomic hardlink write: %+v %v", response, err)
	}
	data, _ = os.ReadFile(target)
	if string(data) != "original" {
		t.Fatal("helper followed hardlink")
	}
}

func TestSeatbeltNetworkAndProxyEnforcement(t *testing.T) {
	ws, _, p := fixture(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	short, err := os.MkdirTemp("/tmp", "claw-unix-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(short)
	unixListener, err := net.Listen("unix", filepath.Join(short, "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer unixListener.Close()
	executable, _ := os.Executable()
	for _, test := range []struct{ network, address string }{{"tcp", listener.Addr().String()}, {"udp", udp.LocalAddr().String()}, {"unix", unixListener.Addr().String()}} {
		t.Run(test.network, func(t *testing.T) {
			if out, err := runCommand(t, p, ws, quoted(executable)+" __net-probe "+test.network+" "+quoted(test.address)); err == nil {
				t.Fatalf("direct network allowed: %s", out)
			}
		})
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "proxied") }))
	defer origin.Close()
	p.Network = sb.NetworkProxy
	p.Domains = map[string]sb.DomainRule{"127.0.0.1": sb.DomainAllow}
	session, err := sb.NewSession(p, sb.ApprovalNever, nil, []string{ws})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for _, protocol := range []string{"http", "socks"} {
		t.Run(protocol, func(t *testing.T) {
			script := "/usr/bin/curl -fsS --max-time 3 " + quoted(origin.URL)
			if protocol == "socks" {
				script = "/usr/bin/curl -fsS --max-time 3 --proxy \"$ALL_PROXY\" " + quoted(origin.URL)
			}
			// curl intentionally ignores uppercase HTTP_PROXY. The lowercase binding
			// and cleared NO_PROXY must make even loopback requests use this proxy.
			cmd, cleanup, err := session.Spawn(context.Background(), []string{"/bin/bash", "-c", script}, ws, p)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			out, err := cmd.CombinedOutput()
			if err != nil || string(out) != "proxied" {
				t.Fatalf("proxy transfer: %s %v", out, err)
			}
		})
	}
	cmd, cleanup, err := session.Spawn(context.Background(), []string{"/usr/bin/curl", "-fsS", "--max-time", "3", "--noproxy", "*", origin.URL}, ws, p)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("proxy bypass: %s", out)
	}
}
func TestSeatbeltLiteralDenyCanBeRefinedAndReadAdditionDoesNotReduceWrite(t *testing.T) {
	ws, _, p := fixture(t)
	dir := filepath.Join(ws, "blocked")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "allowed")
	p.Files = append(p.Files, sb.FileRule{Path: dir, Access: sb.AccessDeny}, sb.FileRule{Path: file, Access: sb.AccessWrite}, sb.FileRule{Path: ws, Access: sb.AccessRead, Approved: true})
	if out, err := runCommand(t, p, ws, "printf ok > "+quoted(file)); err != nil {
		t.Fatalf("specific write denied: %s %v", out, err)
	}
}

func TestSeatbeltDefaultWorkspaceAndProtectedConfig(t *testing.T) {
	requireSeatbelt(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	ws := t.TempDir()
	for _, dir := range []string{filepath.Join(home, ".claw"), filepath.Join(ws, ".claw"), filepath.Join(ws, ".agents"), filepath.Join(ws, ".aws"), filepath.Join(ws, ".git")} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	token := filepath.Join(home, ".claw", "mcp-oauth.json")
	if err := os.WriteFile(token, []byte("test-token"), 0600); err != nil {
		t.Fatal(err)
	}
	profiles, err := sb.BuiltinProfiles(ws)
	if err != nil {
		t.Fatal(err)
	}
	p := profiles[":workspace"]
	for _, dir := range []string{".git", ".claw", ".agents", ".aws"} {
		if out, err := runCommand(t, p, ws, "printf x > "+quoted(filepath.Join(ws, dir, "x"))); err == nil {
			t.Fatalf("protected %s writable: %s", dir, out)
		}
	}
	if out, err := runCommand(t, p, ws, "cat "+quoted(token)); err == nil {
		t.Fatalf("OAuth read allowed: %s", out)
	}
	session, _ := sb.NewSession(p, sb.ApprovalNever, nil, []string{ws})
	defer session.Close()
	executable, _ := os.Executable()
	response, err := fshelper.Run(context.Background(), session, executable, ws, p, fshelper.Request{Op: "read", Path: token, MaxBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	if response.OK {
		t.Fatal("helper OAuth read allowed")
	}
	for _, command := range []string{"printf ok > normal.txt", "git --version", "/usr/bin/grep ok normal.txt"} {
		if out, err := runCommand(t, p, ws, command); err != nil {
			t.Fatalf("normal command %s failed: %s %v", command, out, err)
		}
	}
}
func TestSeatbeltGlobPathEscapingAndPriority(t *testing.T) {
	ws, _, p := fixture(t)
	dir := filepath.Join(ws, `path with space + (x) "quoted"`)
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	env := filepath.Join(dir, "x.env")
	if err := os.WriteFile(env, []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	p.Files = append(p.Files, sb.FileRule{Path: dir, Glob: "**/*.env", Access: sb.AccessDeny}, sb.FileRule{Path: dir, Glob: "**/*", Access: sb.AccessWrite})
	if out, err := runCommand(t, p, ws, "cat "+quoted(env)); err == nil {
		t.Fatalf("escaped glob deny bypassed: %s", out)
	}
	if out, err := runCommand(t, p, ws, "printf ok > "+quoted(filepath.Join(dir, "ok"))); err != nil {
		t.Fatalf("escaped glob write failed: %s %v", out, err)
	}
}
func TestSeatbeltHelperRejectsHardlinkInPlaceFallback(t *testing.T) {
	ws, outside, p := fixture(t)
	original := filepath.Join(outside, "original")
	alias := filepath.Join(ws, "alias")
	if err := os.WriteFile(original, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(original, alias); err != nil {
		t.Fatal(err)
	}
	p.Files = []sb.FileRule{{Path: "/", Access: sb.AccessRead}, {Path: alias, Access: sb.AccessWrite}}
	executable, _ := os.Executable()
	session, _ := sb.NewSession(p, sb.ApprovalNever, nil, nil)
	defer session.Close()
	response, err := fshelper.Run(context.Background(), session, executable, ws, p, fshelper.Request{Op: "write", Path: alias, Content: "changed"})
	if err != nil {
		t.Fatal(err)
	}
	if response.OK {
		t.Fatal("hardlink in-place fallback allowed")
	}
	if data, _ := os.ReadFile(original); string(data) != "original" {
		t.Fatal("original was changed")
	}
}

func TestSeatbeltToolchainCompatibility(t *testing.T) {
	requireSeatbelt(t)
	ws := t.TempDir()
	profiles, err := sb.BuiltinProfiles(ws)
	if err != nil {
		t.Fatal(err)
	}
	p := profiles[":workspace"]
	if err := os.WriteFile(filepath.Join(ws, "input.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", ws, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %s %v", out, err)
	}
	cases := []struct{ name, command string }{{"git", "git status --short && git diff --stat"}, {"rg", "rg hello input.txt"}, {"node", "node -e 'process.stdout.write(\"ok\")'"}, {"python3", "python3 -c 'print(\"ok\"); import subprocess; p=subprocess.Popen([\"/bin/sleep\",\"10\"]); p.terminate(); p.wait(timeout=2)'"}, {"npm", "npm --version"}, {"pip3", "pip3 --version"}}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := exec.LookPath(test.name); err != nil {
				if os.Getenv("CLAW_REQUIRE_TOOLCHAINS") == "1" {
					t.Fatal(err)
				}
				t.Skip("optional toolchain unavailable")
			}
			if out, err := runCommand(t, p, ws, test.command); err != nil {
				t.Fatalf("normal toolchain failed: %s %v", out, err)
			}
		})
	}
	// A tiny package needs no downloaded dependencies and forces compilation.
	if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module acceptance\n\ngo 1.26\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "sample_test.go"), []byte("package acceptance\nimport \"testing\"\nfunc TestOK(t *testing.T) {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Run("go-test", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd, cleanup, err := sb.Prepare(ctx, sb.SpawnRequest{Argv: []string{"/bin/bash", "-c", "GOTOOLCHAIN=local go test ./..."}, Dir: ws, Profile: p})
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go test: %s %v", out, err)
		}
	})
}

func TestSeatbeltReadOnlyToolchainCaches(t *testing.T) {
	ws, outside, p := fixture(t)
	// Cache directories live outside the writable workspace and are explicitly
	// read-only; install targets and fixture packages live inside the workspace.
	npmCache := filepath.Join(outside, "npm-cache")
	pipCache := filepath.Join(outside, "pip-cache")
	for _, dir := range []string{npmCache, pipCache, filepath.Join(ws, "fixture")} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(ws, "package.json"), []byte(`{"name":"cache-test","version":"1.0.0","private":true}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "fixture", "package.json"), []byte(`{"name":"local-fixture","version":"1.0.0"}`), 0644); err != nil {
		t.Fatal(err)
	}
	// A tiny local wheel avoids contacting package indexes or executing builds.
	wheel := filepath.Join(ws, "tiny_fixture-1.0-py3-none-any.whl")
	file, err := os.Create(wheel)
	if err != nil {
		t.Fatal(err)
	}
	z := zip.NewWriter(file)
	for path, content := range map[string]string{"tiny_fixture.py": "VALUE=1\n", "tiny_fixture-1.0.dist-info/METADATA": "Metadata-Version: 2.1\nName: tiny-fixture\nVersion: 1.0\n", "tiny_fixture-1.0.dist-info/WHEEL": "Wheel-Version: 1.0\nGenerator: acceptance\nRoot-Is-Purelib: true\nTag: py3-none-any\n", "tiny_fixture-1.0.dist-info/RECORD": ""} {
		w, err := z.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, command string }{{"npm", "npm install --offline --ignore-scripts --no-audit --no-fund --cache " + quoted(npmCache) + " ./fixture"}, {"pip3", "PIP_DISABLE_PIP_VERSION_CHECK=1 PIP_CACHE_DIR=" + quoted(pipCache) + " pip3 install --no-index --no-deps --target ./pip-target " + quoted(wheel)}} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := exec.LookPath(test.name); err != nil {
				if os.Getenv("CLAW_REQUIRE_TOOLCHAINS") == "1" {
					t.Fatal(err)
				}
				t.Skip("toolchain unavailable")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd, cleanup, err := sb.Prepare(ctx, sb.SpawnRequest{Argv: []string{"/bin/bash", "-c", test.command}, Dir: ws, Profile: p})
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("read-only cache: %s %v", out, err)
			}
		})
	}
	for _, cache := range []string{npmCache, pipCache} {
		entries, err := os.ReadDir(cache)
		if err != nil || len(entries) != 0 {
			t.Fatalf("cache modified: %s entries=%v err=%v", cache, entries, err)
		}
	}
}

func TestSeatbeltGoCacheReadOnly(t *testing.T) {
	requireSeatbelt(t)
	ws := t.TempDir()
	profiles, err := sb.BuiltinProfiles(ws)
	if err != nil {
		t.Fatal(err)
	}
	p := profiles[":workspace"]
	goPath, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	cacheBytes, err := exec.Command(goPath, "env", "GOCACHE").Output()
	if err != nil {
		t.Fatal(err)
	}
	cache, err := sb.NormalizePath(strings.TrimSpace(string(cacheBytes)))
	if err != nil {
		t.Fatal(err)
	}
	p.Files = append(p.Files, sb.FileRule{Path: cache, Access: sb.AccessRead})
	if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module cacheacceptance\n\ngo 1.26\n"), 0644); err != nil {
		t.Fatal(err)
	}
	source := []byte("package cacheacceptance\nimport \"testing\"\nfunc TestFresh(t *testing.T) { t.Log(\"unique freshly compiled source " + filepath.Base(ws) + "\") }\n")
	if err := os.WriteFile(filepath.Join(ws, "fresh_test.go"), source, 0644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd, cleanup, err := sb.Prepare(ctx, sb.SpawnRequest{Argv: []string{goPath, "test", "./..."}, Dir: ws, Profile: p})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Go read-only cache: %s %v", out, err)
	}
}

func TestSeatbeltDetachedDescendantRemainsRestricted(t *testing.T) {
	ws, outside, p := fixture(t)
	target := filepath.Join(outside, "detached")
	done := filepath.Join(ws, "done")
	script := "sleep 0.05; printf changed > " + quoted(target) + "; printf attempted > " + quoted(done)
	command := "nohup /bin/bash -c " + quoted(script) + " >/dev/null 2>&1 &"
	if out, err := runCommand(t, p, ws, command); err != nil {
		t.Fatalf("launch: %s %v", out, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(done); err == nil {
			if _, err := os.Stat(target); !os.IsNotExist(err) {
				t.Fatalf("detached child escaped: %v", err)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("detached child did not attempt operation")
}

func TestSeatbeltHelperStartupMeasurement(t *testing.T) {
	ws, _, p := fixture(t)
	path := filepath.Join(ws, "input")
	if err := os.WriteFile(path, []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}
	session, _ := sb.NewSession(p, sb.ApprovalNever, nil, nil)
	defer session.Close()
	executable, _ := os.Executable()
	durations := make([]time.Duration, 0, 5)
	for i := 0; i < 5; i++ {
		start := time.Now()
		response, err := fshelper.Run(context.Background(), session, executable, ws, p, fshelper.Request{Op: "read", Path: path, MaxBytes: 10})
		if err != nil || !response.OK || response.Content != "ok" {
			t.Fatalf("helper: %+v %v", response, err)
		}
		durations = append(durations, time.Since(start))
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	t.Logf("sandbox-exec + helper single-request latency: median=%s min=%s max=%s", durations[2], durations[0], durations[4])
}

func TestSeatbeltReadApprovalOnlyIncreasesAccess(t *testing.T) {
	ws, _, p := fixture(t)
	file := filepath.Join(ws, "secret.env")
	if err := os.WriteFile(file, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	p.Files = append(p.Files, sb.FileRule{Path: ws, Glob: "**/*.env", Access: sb.AccessDeny}, sb.FileRule{Path: ws, Access: sb.AccessRead, Approved: true})
	if out, err := runCommand(t, p, ws, "printf ok > normal"); err != nil {
		t.Fatalf("read approval removed write: %s %v", out, err)
	}
	if out, err := runCommand(t, p, ws, "cat secret.env"); err != nil || out != "secret" {
		t.Fatalf("approved read: %s %v", out, err)
	}
	if out, err := runCommand(t, p, ws, "printf changed > secret.env"); err == nil {
		t.Fatalf("read approval granted write: %s", out)
	}
	access, _, err := p.ExplainAccess(file)
	if err != nil || access != sb.AccessRead {
		t.Fatalf("diagnostic mismatch: %v %v", access, err)
	}
}
