//go:build linux

package spool

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// openSpoolFile prefers O_TMPFILE on Linux: the file has no directory
// entry, so a crash leaves nothing behind on disk and there is no
// path-based race with another process. When the kernel/filesystem
// returns EOPNOTSUPP / EISDIR / EINVAL we fall back to os.CreateTemp.
//
// The first return value is the writable *os.File. The second is the
// on-disk path that Close() must unlink, or "" if the file is already
// anonymous (O_TMPFILE).
func openSpoolFile() (*os.File, string, error) {
	dir := os.TempDir()
	fd, err := unix.Open(dir, unix.O_TMPFILE|unix.O_RDWR|unix.O_CLOEXEC, 0o600)
	if err == nil {
		return os.NewFile(uintptr(fd), "owui-cee-spool"), "", nil
	}
	// Common fall-through cases on filesystems that do not implement
	// O_TMPFILE (tmpfs supports it; some network mounts do not).
	if errors.Is(err, syscall.EOPNOTSUPP) ||
		errors.Is(err, syscall.EISDIR) ||
		errors.Is(err, syscall.EINVAL) ||
		errors.Is(err, syscall.ENOTSUP) {
		return openSpoolFileFallback()
	}
	return nil, "", err
}

func openSpoolFileFallback() (*os.File, string, error) {
	f, err := os.CreateTemp("", "owui-cee-spool-*")
	if err != nil {
		return nil, "", err
	}
	return f, f.Name(), nil
}
