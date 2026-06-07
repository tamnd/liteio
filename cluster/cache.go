// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tamnd/liteio/cluster/rpc"
	"github.com/vmihailenco/msgpack/v5"
)

// CachePath is the well-known URL path under which a node serves its metacache
// coherence endpoint, reached by a peer at scheme://host + CachePath. Like
// LockPath it must never equal a drive endpoint path.
const CachePath = "/liteio-cache"

// mCacheEvent is the wire name of the single coherence method. It is part of the
// inter-node contract and must stay stable across versions.
const mCacheEvent = "CacheEvent"

// maxInflightBroadcasts bounds the goroutines a broadcaster spends fanning events
// to peers. Past this many in flight, new events are dropped rather than queued:
// a lost event only costs a peer one stale listing until the metacache TTL
// elapses, so coherence is best-effort and never backs up the write path.
const maxInflightBroadcasts = 64

// broadcastTimeout bounds one fan-out so a slow or dead peer cannot pin a
// broadcast slot. It matches the lock service's online-probe budget.
const broadcastTimeout = 2 * time.Second

// cacheEvent is the wire payload: the bucket touched and, for a write, the key
// that was added. An empty key invalidates the whole bucket (a delete).
type cacheEvent struct {
	Bucket string `msgpack:"bucket"`
	Key    string `msgpack:"key"`
}

// CacheSink receives peers' metacache events and applies them to a node's own
// cache. *object.ServerPools satisfies it through ApplyRemoteCache, so the
// inbound half of coherence needs no knowledge of the object layer's internals.
type CacheSink interface {
	ApplyRemoteCache(bucket, key string)
}

// RegisterCache mounts the coherence handler on mux, applying each inbound event
// to sink. It is the server half of cross-node metacache coherence: a node wires
// its object layer as the sink so a peer's write or delete updates this node's
// listing cache.
func RegisterCache(mux *rpc.Mux, sink CacheSink) {
	mux.Register(mCacheEvent, func(_ context.Context, raw msgpack.RawMessage, _ io.Reader, resp *rpc.Response) error {
		var ev cacheEvent
		if err := msgpack.Unmarshal(raw, &ev); err != nil {
			return err
		}
		sink.ApplyRemoteCache(ev.Bucket, ev.Key)
		resp.SetResult(struct{}{})
		return nil
	})
}

// RemoteCache is the client half: it forwards one node's cache events to a peer's
// CachePath over cluster/rpc.
type RemoteCache struct {
	client *rpc.Client
}

// NewRemoteCache targets the peer at base (scheme://host[:port]), appending
// CachePath. A nil hc uses a pooled default client.
func NewRemoteCache(base string, hc *http.Client) *RemoteCache {
	endpoint := strings.TrimRight(base, "/") + CachePath
	return &RemoteCache{client: rpc.NewClient(endpoint, hc)}
}

// Event forwards one coherence event to the peer and drains the reply. A
// non-empty key was added by a write; an empty key invalidates the whole bucket.
func (r *RemoteCache) Event(ctx context.Context, bucket, key string) error {
	reply, err := r.client.Call(ctx, mCacheEvent, cacheEvent{Bucket: bucket, Key: key}, nil)
	if err != nil {
		return err
	}
	return reply.Close()
}

// CacheBroadcaster builds the notifier a node installs with
// object.WithCacheNotifier: it fans every local metacache event out to peers,
// off the write path and best-effort. Each event is delivered from a detached
// goroutine under a short timeout, with the number in flight bounded; once that
// bound is reached further events are dropped and peers re-converge on the next
// listing past the metacache TTL. It returns nil when there are no peers, which
// leaves the layer making no notifications (the single-node case).
func CacheBroadcaster(peers []string, client *http.Client) func(bucket, key string) {
	if len(peers) == 0 {
		return nil
	}
	caches := make([]*RemoteCache, len(peers))
	for i, base := range peers {
		caches[i] = NewRemoteCache(base, client)
	}
	sem := make(chan struct{}, maxInflightBroadcasts)
	return func(bucket, key string) {
		select {
		case sem <- struct{}{}:
		default:
			return // saturated; the TTL backstop re-syncs peers
		}
		go func() {
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(context.Background(), broadcastTimeout)
			defer cancel()
			for _, c := range caches {
				_ = c.Event(ctx, bucket, key)
			}
		}()
	}
}
