//go:build linux

package quota

import (
	"fmt"
	"os"
	"syscall"
)

func openSnapshot(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

func validateSnapshotFile(info os.FileInfo) error {
	if !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("quota: snapshot must be regular and not group/world writable")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (stat.Uid != 0 && stat.Uid != uint32(os.Geteuid())) {
		return fmt.Errorf("quota: snapshot owner must be root or current user")
	}
	return nil
}
