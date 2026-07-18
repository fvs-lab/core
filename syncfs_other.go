//go:build !linux

package core

import "os"

const hasFilesystemSync = false

func syncFilesystem(file *os.File) error {
	return file.Sync()
}
