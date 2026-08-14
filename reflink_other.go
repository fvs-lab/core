//go:build !linux

package core

import (
	"errors"
	"os"
)

var errReflinkUnsupported = errors.New("reflink is unsupported")

func reflinkRange(source, destination *os.File, sourceOffset, destinationOffset, length int64) error {
	return errReflinkUnsupported
}

func enableFilesystemCompression(string) {}
