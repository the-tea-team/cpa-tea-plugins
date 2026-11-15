//go:build !linux

package quota

import (
	"errors"
	"os"
)

// Fail closed where descriptor ownership and no-follow guarantees are not implemented.
func openSnapshot(path string) (*os.File, error) {
	return nil, errors.New("quota: secure snapshot reads are unsupported on this platform")
}

func validateSnapshotFile(info os.FileInfo) error { return nil }
