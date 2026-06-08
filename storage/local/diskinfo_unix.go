// SPDX-License-Identifier: Apache-2.0

//go:build unix

package local

import (
	"context"

	"github.com/tamnd/liteio/storage"
	"golang.org/x/sys/unix"
)

// DiskInfo reports the capacity of the filesystem backing the drive root via
// statfs. Total is the filesystem size; Free is the space available to an
// unprivileged writer (Bavail, which excludes the root-reserved blocks); Used is
// Total minus the truly free blocks. The figures describe the whole filesystem, so
// two drives sharing one filesystem report the same numbers.
func (l *Local) DiskInfo(_ context.Context) (storage.DiskInfo, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(l.root, &st); err != nil {
		return storage.DiskInfo{}, err
	}
	// Bsize is the only field whose type differs across unix platforms (uint32 on
	// darwin, int64 on linux), so it is the one conversion; the block counts are
	// already uint64 everywhere.
	bsize := uint64(st.Bsize)
	total := st.Blocks * bsize
	free := st.Bavail * bsize
	return storage.DiskInfo{
		Total: total,
		Free:  free,
		Used:  total - st.Bfree*bsize,
	}, nil
}
