// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package local

import (
	"context"

	"github.com/tamnd/liteio/storage"
)

// DiskInfo is unsupported on non-unix platforms (no statfs), so it reports the
// sentinel rather than a misleading zero. liteio targets unix in production; this
// keeps the package building everywhere.
func (l *Local) DiskInfo(_ context.Context) (storage.DiskInfo, error) {
	return storage.DiskInfo{}, storage.ErrDiskInfoUnsupported
}
