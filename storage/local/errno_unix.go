// SPDX-License-Identifier: Apache-2.0

//go:build unix

package local

import (
	"errors"
	"syscall"
)

// isNotEmpty reports whether err is "directory not empty".
func isNotEmpty(err error) bool { return errors.Is(err, syscall.ENOTEMPTY) }

// isIsDir reports whether err is "is a directory".
func isIsDir(err error) bool { return errors.Is(err, syscall.EISDIR) }

// isNotSupported reports whether err is an "operation not supported" error, used
// to treat directory fsync as best-effort on filesystems that lack it.
func isNotSupported(err error) bool {
	return errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EINVAL)
}
