package nelmclient

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/werf/nelm/pkg/common"
)

// TestMain points the Docker configuration that nelm's registry client reads
// on every render at an empty directory, for the whole package. isolateHome
// cannot: nelm resolves ~/.docker/config.json
// (common.DefaultRegistryCredentialsPath) when its package is initialized,
// and Docker's configuration directory (DOCKER_CONFIG, else ~/.docker) on
// first use, both from the environment the test binary started with. The
// tests would otherwise read the developer's own Docker configuration, and
// a malformed one would fail every render.
func TestMain(m *testing.M) {
	os.Exit(runIsolatedFromDocker(m))
}

func runIsolatedFromDocker(m *testing.M) int {
	dir, err := os.MkdirTemp("", "nelmclient-docker-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()

	if err := os.Setenv("DOCKER_CONFIG", dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	common.DefaultRegistryCredentialsPath = filepath.Join(dir, "config.json")

	return m.Run()
}
