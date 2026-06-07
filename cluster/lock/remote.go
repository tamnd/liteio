// SPDX-License-Identifier: Apache-2.0

package lock

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/tamnd/liteio/cluster/rpc"
	"github.com/vmihailenco/msgpack/v5"
)

// RPC method names for the lock service. They are part of the wire contract and
// must stay stable across versions.
const (
	mLock        = "Lock"
	mUnlock      = "Unlock"
	mRLock       = "RLock"
	mRUnlock     = "RUnlock"
	mRefresh     = "Refresh"
	mForceUnlock = "ForceUnlock"
	mLockOnline  = "LockOnline"
)

// onlineTimeout bounds the online probe so a dead peer is reported offline
// rather than hanging an acquisition.
const onlineTimeout = 2 * time.Second

// boolResult carries a grant/found decision back over the wire.
type boolResult struct {
	OK bool `msgpack:"ok"`
}

// RemoteLocker is a Locker whose server lives one RPC hop away. Each call
// forwards its Args to the owning node, which runs it against its own
// LocalLocker via the handlers Register installs. It satisfies the same Locker
// contract as LocalLocker, so a DRWMutex's locker set can freely mix local and
// remote servers.
type RemoteLocker struct {
	endpoint string
	client   *rpc.Client
}

var _ Locker = (*RemoteLocker)(nil)

// NewRemoteLocker returns a locker targeting the peer at endpoint
// (scheme://host[:port]). A nil hc uses a pooled default client.
func NewRemoteLocker(endpoint string, hc *http.Client) *RemoteLocker {
	return &RemoteLocker{endpoint: endpoint, client: rpc.NewClient(endpoint, hc)}
}

// String reports the peer endpoint.
func (r *RemoteLocker) String() string { return r.endpoint }

// call invokes one lock method and returns its boolean decision.
func (r *RemoteLocker) call(ctx context.Context, method string, args Args) (bool, error) {
	reply, err := r.client.Call(ctx, method, args, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = reply.Close() }()
	var res boolResult
	if err := reply.Unmarshal(&res); err != nil {
		return false, err
	}
	return res.OK, nil
}

// Lock implements Locker over RPC.
func (r *RemoteLocker) Lock(ctx context.Context, args Args) (bool, error) {
	return r.call(ctx, mLock, args)
}

// Unlock implements Locker over RPC.
func (r *RemoteLocker) Unlock(ctx context.Context, args Args) (bool, error) {
	return r.call(ctx, mUnlock, args)
}

// RLock implements Locker over RPC.
func (r *RemoteLocker) RLock(ctx context.Context, args Args) (bool, error) {
	return r.call(ctx, mRLock, args)
}

// RUnlock implements Locker over RPC.
func (r *RemoteLocker) RUnlock(ctx context.Context, args Args) (bool, error) {
	return r.call(ctx, mRUnlock, args)
}

// Refresh implements Locker over RPC.
func (r *RemoteLocker) Refresh(ctx context.Context, args Args) (bool, error) {
	return r.call(ctx, mRefresh, args)
}

// ForceUnlock implements Locker over RPC.
func (r *RemoteLocker) ForceUnlock(ctx context.Context, args Args) (bool, error) {
	return r.call(ctx, mForceUnlock, args)
}

// IsOnline probes the peer with a short timeout, reporting false on any failure.
func (r *RemoteLocker) IsOnline() bool {
	ctx, cancel := context.WithTimeout(context.Background(), onlineTimeout)
	defer cancel()
	reply, err := r.client.Call(ctx, mLockOnline, struct{}{}, nil)
	if err != nil {
		return false
	}
	_ = reply.Close()
	return true
}

// Register mounts handlers on mux that serve the lock methods from a local
// Locker. This is the server half of a lock node: wrap a LocalLocker, register
// it, and serve the mux over the same HTTP(S) endpoint as the storage RPC.
func Register(mux *rpc.Mux, local Locker) {
	bound := func(method string, fn func(context.Context, Args) (bool, error)) {
		mux.Register(method, func(ctx context.Context, raw msgpack.RawMessage, _ io.Reader, resp *rpc.Response) error {
			var a Args
			if err := msgpack.Unmarshal(raw, &a); err != nil {
				return err
			}
			ok, err := fn(ctx, a)
			if err != nil {
				return err
			}
			resp.SetResult(boolResult{OK: ok})
			return nil
		})
	}
	bound(mLock, local.Lock)
	bound(mUnlock, local.Unlock)
	bound(mRLock, local.RLock)
	bound(mRUnlock, local.RUnlock)
	bound(mRefresh, local.Refresh)
	bound(mForceUnlock, local.ForceUnlock)
	mux.Register(mLockOnline, func(_ context.Context, _ msgpack.RawMessage, _ io.Reader, resp *rpc.Response) error {
		resp.SetResult(boolResult{OK: true})
		return nil
	})
}
