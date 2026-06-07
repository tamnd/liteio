// SPDX-License-Identifier: Apache-2.0

package object

import (
	"context"

	"github.com/tamnd/liteio/cluster/lock"
)

// nsLockSource labels a namespace-lock acquisition in diagnostics.
const nsLockSource = "object"

// nsResource is the lock key for one object: "bucket/object". Every mutating
// operation on the same key contends on the same resource across the cluster.
func nsResource(bucket, object string) string { return bucket + "/" + object }

// lockObject takes the exclusive namespace lock for a mutating operation on
// bucket/object and returns an unlock function. If the context is done before a
// quorum grants, it returns ErrOperationTimedOut and a nil unlock. A single-node
// deployment holds one local locker, so this is an in-process write lock; a
// clustered deployment holds a quorum of lock servers over cluster/rpc, all behind
// the same Locker contract.
func (sp *ServerPools) lockObject(ctx context.Context, bucket, object string) (func(), error) {
	m := lock.NewDRWMutex(sp.nodeID, sp.nsLockers, nsResource(bucket, object))
	if !m.GetLock(ctx, nsLockSource) {
		return nil, ErrOperationTimedOut
	}
	return m.Unlock, nil
}
