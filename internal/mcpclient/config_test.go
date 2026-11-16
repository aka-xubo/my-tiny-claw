package mcpclient

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfigMissingFile(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || cfg != nil {
		t.Fatalf("缺失配置应返回 nil, nil: %v %v", cfg, err)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	cases := map[string]string{
		"非法 JSON":         `{`,
		"尾部多余内容":          `{"servers":{}} garbage`,
		"多个 JSON 值":       `{"servers":{}} {"servers":{}}`,
		"未知字段":            `{"servers":{"a":{"transport":"streamable-http","url":"https://x","command":"ls"}}}`,
		"非法名称":            `{"servers":{"a_b":{"transport":"streamable-http","url":"https://x"}}}`,
		"非法 transport":    `{"servers":{"a":{"transport":"stdio","url":"https://x"}}}`,
		"非法 scheme":       `{"servers":{"a":{"transport":"streamable-http","url":"file:///etc/passwd"}}}`,
		"缺少 url":          `{"servers":{"a":{"transport":"stateless-http"}}}`,
		"缺失环境变量":          `{"servers":{"a":{"transport":"streamable-http","url":"https://x","headers":{"X":"${CLAW_TEST_MISSING_VAR}"}}}}`,
		"oauth 与 Auth 冲突": `{"servers":{"a":{"transport":"streamable-http","url":"https://x","oauth":{},"headers":{"authorization":"Bearer t"}}}}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			p := writeConfig(t, content)
			_, err := LoadConfig(p)
			if err == nil {
				t.Fatal("期望报错")
			}
			if !strings.Contains(err.Error(), p) {
				t.Fatalf("错误信息应包含配置路径: %v", err)
			}
		})
	}
}

func TestLoadConfigExpandsEnvWithoutLeakingValue(t *testing.T) {
	t.Setenv("CLAW_TEST_TOKEN", "s3cret")
	p := writeConfig(t, `{"servers":{"gh":{"transport":"streamable-http","url":"https://x","headers":{"Authorization":"Bearer ${CLAW_TEST_TOKEN}"}}}}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Servers["gh"].Headers["Authorization"]; got != "Bearer s3cret" {
		t.Fatalf("环境变量展开错误: %q", got)
	}

	p = writeConfig(t, `{"servers":{"gh":{"transport":"streamable-http","url":"https://x","headers":{"A":"${CLAW_TEST_TOKEN}","B":"${CLAW_TEST_MISSING_VAR}"}}}}`)
	_, err = LoadConfig(p)
	if err == nil || strings.Contains(err.Error(), "s3cret") || !strings.Contains(err.Error(), "CLAW_TEST_MISSING_VAR") {
		t.Fatalf("错误应包含变量名且不含任何值: %v", err)
	}
}

func TestTimeoutDefaults(t *testing.T) {
	sc := ServerConfig{}
	if sc.connectTimeout() != defaultConnectTimeout || sc.callTimeout() != defaultCallTimeout {
		t.Fatal("默认超时错误")
	}
	sc.OAuth = &OAuthConfig{}
	if sc.connectTimeout() != defaultAuthTimeout {
		t.Fatal("OAuth Server 应使用授权超时")
	}
	if sc.redirectURL() != "http://127.0.0.1:19876/callback" {
		t.Fatalf("默认回调地址错误: %s", sc.redirectURL())
	}
}
