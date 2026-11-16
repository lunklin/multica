//go:build !darwin && !linux

package storage

import (
	"fmt"
	"os"
)

func localDirectoryIdentity(os.FileInfo) (string, error) {
	return "", fmt.Errorf("durable local namespace identity unsupported on this platform")
}
