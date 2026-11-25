//go:build !darwin

package sandbox

import (
	"context"
	"os"
	"os/exec"
)

func Prepare(ctx context.Context, req SpawnRequest) (*exec.Cmd, func(), error) {
	if err := validateSpawnRequest(req); err != nil {
		return nil, nil, err
	}
	if req.Env == nil {
		req.Env = os.Environ()
	}
	if !req.Profile.FullAccess {
		return nil, nil, ErrUnsupported
	}
	return prepareCommand(ctx, req, req.Argv), func() {}, nil
}
