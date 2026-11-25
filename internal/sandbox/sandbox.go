package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type SpawnRequest struct {
	Argv          []string
	Dir           string
	Env           []string
	Profile       *Profile
	Proxy         *ProxyEndpoints
	WritableRoots []string
	EnvExclude    []string
	EnvKeep       []string
}

type ProxyEndpoints struct {
	HTTPPort  int
	SOCKSPort int
}

var ErrUnsupported = errors.New("当前平台不支持该权限档的沙箱，已拒绝执行")

func validateSpawnRequest(req SpawnRequest) error {
	if len(req.Argv) == 0 || req.Argv[0] == "" {
		return errors.New("Argv 不能为空，且 Argv[0] 必须是绝对路径")
	}
	if !isAbsolute(req.Argv[0]) {
		return fmt.Errorf("Argv[0] 必须是绝对路径: %s", req.Argv[0])
	}
	if req.Profile == nil {
		return errors.New("权限档不能为空")
	}
	return nil
}

func prepareCommand(ctx context.Context, req SpawnRequest, command []string) *exec.Cmd {
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Dir = req.Dir
	env := req.Env
	if env == nil {
		env = []string{}
	}
	cmd.Env = CleanEnvironment(env, req.EnvExclude, req.EnvKeep)
	return cmd
}

func withProxyEnvironment(env []string, endpoints *ProxyEndpoints) []string {
	if endpoints == nil {
		return env
	}
	result := make([]string, 0, len(env)+10)
	for _, entry := range env {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || strings.EqualFold(name, "NO_PROXY") || strings.EqualFold(name, "no_proxy") || isProxyEnvironmentName(name) {
			continue
		}
		result = append(result, entry)
	}
	httpProxy := "http://127.0.0.1:" + strconv.Itoa(endpoints.HTTPPort)
	socksProxy := "socks5h://127.0.0.1:" + strconv.Itoa(endpoints.SOCKSPort)
	for _, entry := range []string{
		"HTTP_PROXY=" + httpProxy, "HTTPS_PROXY=" + httpProxy,
		"http_proxy=" + httpProxy, "https_proxy=" + httpProxy,
		"ALL_PROXY=" + socksProxy, "all_proxy=" + socksProxy,
		"PIP_PROXY=" + httpProxy, "npm_config_proxy=" + httpProxy,
		"npm_config_https_proxy=" + httpProxy, "YARN_HTTP_PROXY=" + httpProxy,
		"YARN_HTTPS_PROXY=" + httpProxy,
	} {
		result = append(result, entry)
	}
	return result
}

func isProxyEnvironmentName(name string) bool {
	for _, prefix := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "PIP_PROXY", "npm_config_proxy", "npm_config_https_proxy", "YARN_HTTP_PROXY", "YARN_HTTPS_PROXY"} {
		if strings.EqualFold(name, prefix) {
			return true
		}
	}
	return false
}

func isAbsolute(path string) bool {
	return filepath.IsAbs(path)
}
