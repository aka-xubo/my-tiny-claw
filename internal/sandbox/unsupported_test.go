//go:build !darwin

package sandbox

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestRestrictedProfilesFailClosedOnUnsupportedPlatform(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd, _, err := Prepare(context.Background(), SpawnRequest{Argv: []string{executable}, Profile: &Profile{Name: ":workspace"}})
	if !errors.Is(err, ErrUnsupported) || cmd != nil {
		t.Fatalf("restricted: cmd=%v err=%v", cmd, err)
	}
	cmd, cleanup, err := Prepare(context.Background(), SpawnRequest{Argv: []string{executable}, Profile: &Profile{FullAccess: true}})
	if err != nil || cmd == nil {
		t.Fatalf("full access: %v", err)
	}
	cleanup()
}
