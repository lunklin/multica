//go:build darwin || linux

package storage

import (
	"fmt"
	"os"
	"syscall"
)

func localDirectoryIdentity(info os.FileInfo) (string, error) {
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("directory identity unavailable")
	}
	return fmt.Sprintf("%d:%d", uint64(s.Dev), uint64(s.Ino)), nil
}
