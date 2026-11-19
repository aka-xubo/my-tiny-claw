package tools

import (
	"context"
	"path/filepath"
)

type executionContextKey string

const (
	ownerIDContextKey executionContextKey = "my-tiny-claw.owner-id"
	workDirContextKey executionContextKey = "my-tiny-claw.work-dir"
)

// WithExecutionContext attaches the logical owner and workspace used for
// managed bash sessions to a tool invocation.
func WithExecutionContext(ctx context.Context, ownerID, workDir string) context.Context {
	if workDir != "" {
		if abs, err := filepath.Abs(workDir); err == nil {
			workDir = filepath.Clean(abs)
		}
	}
	ctx = context.WithValue(ctx, ownerIDContextKey, ownerID)
	return context.WithValue(ctx, workDirContextKey, workDir)
}

func executionOwner(ctx context.Context) string {
	if value, ok := ctx.Value(ownerIDContextKey).(string); ok && value != "" {
		return value
	}
	return "default"
}

func executionWorkDir(ctx context.Context, fallback string) string {
	if value, ok := ctx.Value(workDirContextKey).(string); ok && value != "" {
		return value
	}
	if abs, err := filepath.Abs(fallback); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(fallback)
}
