//go:build linux

package core

import (
	"os"

	"golang.org/x/sys/unix"
)

const hasFilesystemSync = true

func syncFilesystem(file *os.File) error {
	return unix.Syncfs(int(file.Fd()))
}
