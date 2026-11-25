package sandbox

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestSBPLGolden(t *testing.T) {
	readOnly := &Profile{Name: ":read-only", Files: []FileRule{{Path: "/", Access: AccessRead}, {Path: "/home/user/.claw/mcp-oauth.json", Access: AccessDeny}}}
	workspace := cloneProfile(readOnly)
	workspace.Name = ":workspace"
	workspace.ProtectedRoots = []string{"/workspace"}
	workspace.Files = append(workspace.Files, FileRule{Path: "/workspace", Access: AccessWrite}, FileRule{Path: "/workspace/.git", Access: AccessRead}, FileRule{Path: "/private/tmp", Access: AccessWrite})
	dev := cloneProfile(workspace)
	dev.Network = NetworkProxy
	dev.Files = append(dev.Files, FileRule{Path: "/workspace", Glob: "**/*.env", Access: AccessDeny}, FileRule{Path: "/workspace", Glob: "config/**", Access: AccessRead})
	approved := cloneProfile(dev)
	approved.Files = append(approved.Files, FileRule{Path: "/workspace/.git", Access: AccessWrite, Approved: true})
	for name, p := range map[string]*Profile{"read-only": readOnly, "workspace": workspace, "dev": dev, "approved": approved} {
		t.Run(name, func(t *testing.T) {
			policy, args, err := CompileSBPL(p, &ProxyEndpoints{HTTPPort: 12345, SOCKSPort: 12346})
			if err != nil {
				t.Fatal(err)
			}
			actual := policy + "\n# argv\n" + strings.Join(args, "\n") + "\n"
			path := filepath.Join("testdata", name+".golden")
			if os.Getenv("UPDATE_GOLDEN") == "1" {
				if err := os.MkdirAll("testdata", 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(actual), 0644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if actual != string(want) {
				t.Fatalf("policy golden differs: %s", path)
			}
		})
	}
}
func TestGlobRegexMatchesOnlyIntendedTree(t *testing.T) {
	for _, test := range []struct {
		glob, path string
		want       bool
	}{
		{"**/*.env", "/workspace/prod.env", true}, {"**/*.env", "/workspace/a/b/dev.env", true}, {"**/*.env", "/workspace/a.env/sub/file", true}, {"**/*.env", "/workspace2/prod.env", false}, {"*.env", "/workspace/config/prod.env", false}, {"a?.go", "/workspace/ab.go", true}, {"a?.go", "/workspace/abc.go", false},
	} {
		expression, err := GlobToRegex("/workspace", test.glob)
		if err != nil {
			t.Fatal(err)
		}
		r, err := regexp.Compile(expression)
		if err != nil {
			t.Fatal(err)
		}
		if r.MatchString(test.path) != test.want {
			t.Errorf("%s %s", test.glob, test.path)
		}
	}
}
