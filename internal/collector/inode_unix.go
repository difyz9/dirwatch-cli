//go:build linux || darwin

package collector

import (
	"os"
	"syscall"
)

func inodeOf(info os.FileInfo) uint64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return stat.Ino
	}
	return 0
}
