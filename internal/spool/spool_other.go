//go:build !linux

package spool

import "os"

// openSpoolFile on non-Linux platforms always uses os.CreateTemp. The
// caller is responsible for unlinking via the returned path on Close.
func openSpoolFile() (*os.File, string, error) {
	f, err := os.CreateTemp("", "owui-cee-spool-*")
	if err != nil {
		return nil, "", err
	}
	return f, f.Name(), nil
}
