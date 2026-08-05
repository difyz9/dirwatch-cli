//go:build windows

package collector

import "os"

func inodeOf(info os.FileInfo) uint64 {
	return 0
}
