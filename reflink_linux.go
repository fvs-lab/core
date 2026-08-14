//go:build linux

package core

import (
	"os"

	"golang.org/x/sys/unix"
)

const fsCompressionFlag = 0x00000004

func reflinkRange(source, destination *os.File, sourceOffset, destinationOffset, length int64) error {
	return unix.IoctlFileCloneRange(int(destination.Fd()), &unix.FileCloneRange{
		Src_fd:      int64(source.Fd()),
		Src_offset:  uint64(sourceOffset),
		Src_length:  uint64(length),
		Dest_offset: uint64(destinationOffset),
	})
}

func enableFilesystemCompression(path string) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()
	flags, err := unix.IoctlGetInt(int(file.Fd()), unix.FS_IOC_GETFLAGS)
	if err != nil || flags&fsCompressionFlag != 0 {
		return
	}
	_ = unix.IoctlSetPointerInt(int(file.Fd()), unix.FS_IOC_SETFLAGS, flags|fsCompressionFlag)
}
