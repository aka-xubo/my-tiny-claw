package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/aka-xubo/my-tiny-claw/internal/fshelper"
	"github.com/aka-xubo/my-tiny-claw/internal/sandbox"
)

func defaultSandboxSession(workDir string) (*sandbox.Session, error) {
	profiles, err := sandbox.BuiltinProfiles(workDir)
	if err != nil {
		return nil, err
	}
	return sandbox.NewSession(profiles[":workspace"], sandbox.ApprovalOnRequest, sandbox.DenyApprover{}, []string{workDir})
}

func helperPath() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("获取 claw 可执行文件路径失败: %w", err)
	}
	return filepath.EvalSymlinks(executable)
}

func resolveToolPath(workDir, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(workDir, path))
}

func runFileHelper(ctx context.Context, session *sandbox.Session, profile *sandbox.Profile, workDir string, request fshelper.Request) (fshelper.Response, error) {
	executable, err := helperPath()
	if err != nil {
		return fshelper.Response{}, err
	}
	helpCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	response, err := fshelper.Run(helpCtx, session, executable, workDir, profile, request)
	if err != nil {
		return fshelper.Response{}, err
	}
	if !response.OK {
		message := response.Error
		if message == "" {
			message = "文件操作失败"
		}
		if response.Errno != "" {
			message += " (" + response.Errno + ")"
		}
		if response.Errno == "EPERM" {
			access, rule, err := profile.ExplainAccess(request.Path)
			if err == nil && (access == sandbox.AccessDeny || (request.Op != "read" && access == sandbox.AccessRead)) {
				message += "\n[sandbox] 访问被权限档 " + profile.Name + " 拒绝（规则：" + rule + "）。"
			}
		}
		return response, errors.New(message)
	}
	return response, nil
}
