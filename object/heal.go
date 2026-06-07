// SPDX-License-Identifier: Apache-2.0

package object

import (
	"bytes"
	"context"
	"path"
	"strconv"
	"sync/atomic"

	"github.com/tamnd/liteio/object/erasure"
	"github.com/tamnd/liteio/object/meta"
)

// DefaultMRFDepth is the number of pending heal tasks the most-recently-failed
// queue buffers before it starts dropping. A drop is not a loss of durability:
// the object already reached write quorum, and the proactive scanner (a later
// milestone) re-heals anything reactive heal misses. The depth only bounds the
// burst the reactive path absorbs.
const DefaultMRFDepth = 4096

// healTask names one object version to repair onto the drives in its set that
// are missing it. The write path enqueues a task when an object committed to
// quorum but not to every drive, because a drive was down during the write.
type healTask struct {
	bucket    string
	object    string
	versionID string
}

// mrf is the most-recently-failed queue: a bounded, lossy buffer of heal tasks
// drained by a single background worker. It is deliberately best-effort. A full
// buffer drops new tasks (counted, not blocked) so a write is never slowed by
// healing, and the proactive scanner is the durable backstop.
type mrf struct {
	tasks   chan healTask
	heal    func(context.Context, healTask) error
	dropped atomic.Int64
	healed  atomic.Int64
	failed  atomic.Int64
}

// newMRF builds a queue of the given depth whose worker calls heal for each task.
func newMRF(heal func(context.Context, healTask) error, depth int) *mrf {
	if depth < 1 {
		depth = DefaultMRFDepth
	}
	return &mrf{tasks: make(chan healTask, depth), heal: heal}
}

// enqueue offers a task to the queue without blocking. If the buffer is full the
// task is dropped and counted, so the hot write path is never stalled by heal
// backpressure.
func (m *mrf) enqueue(t healTask) {
	select {
	case m.tasks <- t:
	default:
		m.dropped.Add(1)
	}
}

// run drains the queue until ctx is cancelled, healing each task. Errors are
// counted, not retried inline; the proactive scanner re-attempts anything that
// keeps failing.
func (m *mrf) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-m.tasks:
			if err := m.heal(ctx, t); err != nil {
				m.failed.Add(1)
			} else {
				m.healed.Add(1)
			}
		}
	}
}

// MRFStats reports the reactive-heal queue's running counters.
type MRFStats struct {
	Dropped int64 // tasks discarded because the buffer was full
	Healed  int64 // tasks healed successfully
	Failed  int64 // tasks the worker could not heal
}

// maybeHeal queues a reactive heal when a write reached quorum (okCount drives)
// but missed at least one drive, so the laggards are repaired in the background.
// It is a no-op until the owning ServerPools wires notifyPartial.
func (s *erasureSet) maybeHeal(okCount int, bucket, object, versionID string) {
	if s.notifyPartial != nil && okCount < len(s.drives) {
		s.notifyPartial(bucket, object, versionID)
	}
}

// healObject reconstructs version versionID of bucket/object onto every drive in
// the set that is missing it, reading the surviving shards, decoding the data,
// re-encoding the full shard set, and writing the missing drives' shards and
// obj.meta. An empty versionID heals the latest version. It is a no-op when the
// version is already on every drive, is a delete marker, or no longer exists.
//
// It heals whole-drive gaps — a drive that held no copy of the version because
// it was down during the write. A surviving but bit-rotted shard on an otherwise
// present drive is the proactive scanner's job; reactive heal keys off whether a
// drive carries the version in its obj.meta at all.
func (s *erasureSet) healObject(ctx context.Context, bucket, object, versionID string) error {
	metas := s.readAllMeta(ctx, bucket, object)
	selected, present, ok := meta.QuorumVersion(metas, versionID, s.readQuorum())
	if !ok {
		// Too few healthy copies survive to reconstruct from; leave it for the
		// scanner rather than risk writing a wrong shard.
		return ErrReadQuorum
	}
	rep, found := firstPresent(selected)
	if !found || rep.Deleted {
		return nil
	}

	var missing []int
	for d := range s.drives {
		if !present[d] {
			missing = append(missing, d)
		}
	}
	if len(missing) == 0 {
		return nil // already fully replicated
	}

	k := rep.Erasure.DataBlocks
	m := rep.Erasure.ParityBlocks
	n := k + m
	if n != len(s.drives) || len(rep.Erasure.Distribution) != n {
		return ErrInvalidArgument
	}
	coder, err := erasure.NewCoder(k, m)
	if err != nil {
		return err
	}

	// Reconstruct every part's full shard set from the survivors.
	full := make([][][]byte, len(rep.Parts))
	sums := make([][][]byte, len(rep.Parts))
	for pi, part := range rep.Parts {
		shards := make([][]byte, n)
		for d := range s.drives {
			if !present[d] {
				continue
			}
			fi := selected[d]
			li := fi.Erasure.Index
			if li < 0 || li >= n {
				continue
			}
			shard, rerr := s.readPartShard(ctx, d, bucket, object, fi, part.Number)
			if rerr != nil {
				continue
			}
			if pi < len(fi.Parts) && len(fi.Parts[pi].Checksums) == 1 {
				if !erasure.VerifyShard(shard, fi.Parts[pi].Checksums[0]) {
					continue
				}
			}
			shards[li] = shard
		}
		partData, derr := erasure.DecodeData(coder, shards, int(part.Size))
		if derr != nil {
			return ErrReadQuorum
		}
		enc, eerr := erasure.EncodeData(coder, partData)
		if eerr != nil {
			return eerr
		}
		full[pi] = make([][]byte, n)
		sums[pi] = make([][]byte, n)
		for i := range enc {
			full[pi][i] = append([]byte(nil), enc[i]...)
			sums[pi][i] = erasure.HashShard(full[pi][i])
		}
	}

	inline := rep.IsInline()
	for _, d := range missing {
		li := distIndex(rep.Erasure.Distribution, d)
		if li < 0 {
			continue
		}
		if err := s.healDrive(ctx, d, li, bucket, object, rep, full, sums, inline, metas[d], versionID != ""); err != nil {
			return err
		}
	}
	return nil
}

// healDrive writes the reconstructed shard(s) for erasure index li and the merged
// obj.meta onto drive d. For an inline object the shard rides in obj.meta; for an
// out-of-line object the part files are staged and committed with RenameData
// before the meta is written, so a crash never leaves meta pointing at absent
// shards.
func (s *erasureSet) healDrive(ctx context.Context, d, li int, bucket, object string, rep meta.FileInfo, full, sums [][][]byte, inline bool, existing []meta.FileInfo, versioned bool) error {
	drive := s.drives[d]

	fi := rep
	fi.Erasure.Index = li
	fi.Parts = make([]meta.ObjectPartInfo, len(rep.Parts))
	for pi, part := range rep.Parts {
		fi.Parts[pi] = part
		fi.Parts[pi].Checksums = [][]byte{sums[pi][li]}
	}

	if inline {
		fi.InlineData = full[0][li]
	} else {
		fi.InlineData = nil
		vdir := versionDir(fi.VersionID)
		staging := path.Join(reserved, "tmp", "heal-"+fi.ETag+"-"+strconv.FormatInt(fi.ModTime.UnixNano(), 10)+"-"+strconv.Itoa(li))
		for pi, part := range rep.Parts {
			stagePart := path.Join(staging, partFile(part.Number))
			shard := full[pi][li]
			if err := drive.CreateFile(ctx, bucket, stagePart, int64(len(shard)), bytes.NewReader(shard)); err != nil {
				return err
			}
		}
		if err := drive.RenameData(ctx, bucket, staging, path.Join(object, vdir)); err != nil {
			return err
		}
	}

	versions := meta.AddObjectVersion(existing, fi, versioned)
	raw, err := meta.Marshal(versions)
	if err != nil {
		return err
	}
	return drive.WriteMeta(ctx, bucket, object, raw)
}

// distIndex returns the erasure index li whose shard belongs on physical drive
// position p (the li with Distribution[li] == p), or -1 if p is not placed.
func distIndex(dist []int, p int) int {
	for li, phys := range dist {
		if phys == p {
			return li
		}
	}
	return -1
}
