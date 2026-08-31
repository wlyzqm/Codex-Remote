//go:build !linux

package server

import (
	"errors"
	"os"
)

var errArtifactPathChanged = errors.New("secure artifact opening requires Linux openat2")

// The receiver is deployed on Linux. Other platforms fail closed rather than
// silently restoring the path-check/path-open race.
func openArtifactFile(string) (*os.File, error) {
	return nil, errArtifactPathChanged
}
