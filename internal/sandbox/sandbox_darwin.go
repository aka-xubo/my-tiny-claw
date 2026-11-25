//go:build darwin

package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

func Prepare(ctx context.Context, req SpawnRequest) (*exec.Cmd, func(), error) {
	if err := validateSpawnRequest(req); err != nil {
		return nil, nil, err
	}
	if req.Dir == "" {
		req.Dir = "."
	}
	if req.Env == nil {
		req.Env = os.Environ()
	}
	if req.Profile.Network == NetworkProxy {
		req.Env = withProxyEnvironment(req.Env, req.Proxy)
	}
	if req.Profile.FullAccess {
		return prepareCommand(ctx, req, req.Argv), func() {}, nil
	}
	if req.Profile.Network == NetworkProxy && req.Proxy == nil {
		return nil, nil, fmt.Errorf("proxy 网络模式缺少代理端点，已拒绝执行")
	}
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
		return nil, nil, fmt.Errorf("sandbox-exec 不可用，已拒绝执行: %w", err)
	}
	normalized := cloneProfile(req.Profile)
	for i := range normalized.Files {
		path, err := NormalizePath(normalized.Files[i].Path)
		if err != nil {
			return nil, nil, err
		}
		normalized.Files[i].Path = path
	}
	for i := range normalized.ProtectedRoots {
		path, err := NormalizePath(normalized.ProtectedRoots[i])
		if err != nil {
			return nil, nil, err
		}
		normalized.ProtectedRoots[i] = path
	}
	profile, pathArgs, err := CompileSBPL(normalized, req.Proxy)
	if err != nil {
		return nil, nil, fmt.Errorf("生成 Seatbelt 规则失败: %w", err)
	}
	argv := []string{"/usr/bin/sandbox-exec", "-p", profile}
	argv = append(argv, pathArgs...)
	argv = append(argv, "--")
	argv = append(argv, req.Argv...)
	return prepareCommand(ctx, req, argv), func() {}, nil
}
