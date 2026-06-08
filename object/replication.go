// SPDX-License-Identifier: Apache-2.0

package object

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/tamnd/liteio/object/meta"
	"github.com/tamnd/liteio/replication"
)

// --- bucket replication config CRUD ------------------------------------

// SetBucketReplication implements ObjectLayer: stores the replication config.
func (sp *ServerPools) SetBucketReplication(ctx context.Context, bucket string, cfg replication.ReplicationConfig) error {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return err
	}
	doc, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	for _, set := range sp.allSets() {
		if err := set.setBucketReplication(ctx, bucket, doc); err != nil {
			return err
		}
	}
	return nil
}

// GetBucketReplication implements ObjectLayer: returns the replication config,
// or ErrNoSuchBucketReplication when none is set.
func (sp *ServerPools) GetBucketReplication(ctx context.Context, bucket string) (replication.ReplicationConfig, error) {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return replication.ReplicationConfig{}, err
	}
	doc, err := sp.allSets()[0].bucketReplication(ctx, bucket)
	if err != nil {
		return replication.ReplicationConfig{}, ErrNoSuchBucketReplication
	}
	var cfg replication.ReplicationConfig
	if err := json.Unmarshal(doc, &cfg); err != nil {
		return replication.ReplicationConfig{}, ErrNoSuchBucketReplication
	}
	return cfg, nil
}

// DeleteBucketReplication implements ObjectLayer: removes the replication config.
func (sp *ServerPools) DeleteBucketReplication(ctx context.Context, bucket string) error {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return err
	}
	for _, set := range sp.allSets() {
		if err := set.deleteBucketReplication(ctx, bucket); err != nil {
			return err
		}
	}
	return nil
}

// --- replication dispatch -----------------------------------------------

// maybeReplicate is called after a successful PutObject. When the bucket has a
// replication config and the object is not already a REPLICA, it fans out the
// object to all matching destination rules in a background goroutine.
func (sp *ServerPools) maybeReplicate(ctx context.Context, bucket, object string, oi ObjectInfo) {
	cfg, err := sp.GetBucketReplication(ctx, bucket)
	if err != nil || cfg.IsEmpty() {
		return
	}
	isReplica := oi.UserDefined[replication.MetaStatus] == string(replication.StatusReplica)
	rules := cfg.MatchingRules(object, isReplica)
	if len(rules) == 0 {
		return
	}
	// Fire-and-forget: update status to PENDING synchronously, then replicate in
	// a goroutine. The goroutine updates to COMPLETE or FAILED.
	_ = sp.route(object).setReplicationStatus(ctx, bucket, object, oi.VersionID, replication.StatusPending)
	go sp.replicateObject(context.Background(), bucket, object, oi, rules)
}

// maybeReplicateDelete is called after a successful DeleteObject. It replicates
// the deletion to all matching rules that have DeleteReplication enabled.
func (sp *ServerPools) maybeReplicateDelete(ctx context.Context, bucket, object, versionID string, isDeleteMarker bool) {
	cfg, err := sp.GetBucketReplication(ctx, bucket)
	if err != nil || cfg.IsEmpty() {
		return
	}
	rules := cfg.MatchingRules(object, false)
	if len(rules) == 0 {
		return
	}
	go sp.replicateDelete(context.Background(), bucket, object, versionID, isDeleteMarker, rules)
}

// replicateObject sends the object to each destination rule.
func (sp *ServerPools) replicateObject(ctx context.Context, bucket, object string, oi ObjectInfo, rules []replication.Rule) {
	// Read object data once.
	reader, err := sp.route(object).getObject(ctx, bucket, object, ObjectOptions{VersionID: oi.VersionID})
	if err != nil {
		_ = sp.route(object).setReplicationStatus(ctx, bucket, object, oi.VersionID, replication.StatusFailed)
		return
	}
	defer reader.Close() //nolint:errcheck
	data, err := io.ReadAll(reader)
	if err != nil {
		_ = sp.route(object).setReplicationStatus(ctx, bucket, object, oi.VersionID, replication.StatusFailed)
		return
	}

	anyFailed := false
	for _, rule := range rules {
		client := replication.NewClient(rule.Destination, bucket)
		userMeta := stripReplicationMeta(oi.UserDefined)
		putErr := client.PutObject(ctx, bucket, object, bytes.NewReader(data), int64(len(data)), userMeta, true)
		if putErr != nil {
			anyFailed = true
		}
	}
	status := replication.StatusComplete
	if anyFailed {
		status = replication.StatusFailed
	}
	_ = sp.route(object).setReplicationStatus(ctx, bucket, object, oi.VersionID, status)
}

// replicateDelete sends a DELETE to each destination rule that has
// DeleteReplication enabled.
func (sp *ServerPools) replicateDelete(ctx context.Context, bucket, object, versionID string, isDeleteMarker bool, rules []replication.Rule) {
	for _, rule := range rules {
		if !rule.DeleteReplication {
			continue
		}
		client := replication.NewClient(rule.Destination, bucket)
		_ = client.DeleteObject(ctx, bucket, object, versionID)
	}
}

// setReplicationStatus updates the replication status on the live version
// without touching the object data.
func (s *erasureSet) setReplicationStatus(ctx context.Context, bucket, object, versionID string, status replication.Status) error {
	metas := s.readAllMeta(ctx, bucket, object)
	selected, _, ok := meta.QuorumVersion(metas, versionID, s.readQuorum())
	if !ok {
		return ErrObjectNotFound
	}
	fi, found := firstPresent(selected)
	if !found || fi.Deleted {
		return ErrObjectNotFound
	}
	pinned := fi.VersionID
	writes := fanOut(ctx, len(s.drives), func(ctx context.Context, i int) (struct{}, error) {
		versions := metas[i]
		if versions == nil {
			return struct{}{}, nil
		}
		for j := range versions {
			if versions[j].VersionID != pinned {
				continue
			}
			if versions[j].Metadata == nil {
				versions[j].Metadata = map[string]string{}
			}
			versions[j].Metadata[replication.MetaStatus] = string(status)
			raw, err := meta.Marshal(versions)
			if err != nil {
				return struct{}{}, err
			}
			return struct{}{}, s.drives[i].WriteMeta(ctx, bucket, object, raw)
		}
		return struct{}{}, nil
	})
	if countOK(writes) < s.writeQuorum() {
		return ErrWriteQuorum
	}
	return nil
}

// stripReplicationMeta removes internal liteio replication metadata keys from
// user-defined metadata before sending to the destination so the replica
// does not inherit PENDING/COMPLETE status from the source.
func stripReplicationMeta(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if k == replication.MetaStatus || k == replication.MetaSourceCluster {
			continue
		}
		out[k] = v
	}
	return out
}

// --- erasureSet helpers -------------------------------------------------

func (s *erasureSet) replicationPath() string { return joinPath(reserved, "replication") }

func (s *erasureSet) setBucketReplication(ctx context.Context, bucket string, doc []byte) error {
	res := fanOut(ctx, len(s.drives), func(ctx context.Context, i int) (struct{}, error) {
		return struct{}{}, s.drives[i].WriteMeta(ctx, bucket, s.replicationPath(), doc)
	})
	if countOK(res) < s.writeQuorum() {
		return ErrWriteQuorum
	}
	return nil
}

func (s *erasureSet) bucketReplication(ctx context.Context, bucket string) ([]byte, error) {
	for _, d := range s.drives {
		if !d.IsOnline() {
			continue
		}
		data, err := d.ReadMeta(ctx, bucket, s.replicationPath())
		if err != nil {
			continue
		}
		return data, nil
	}
	return nil, ErrNoSuchBucketReplication
}

func (s *erasureSet) deleteBucketReplication(ctx context.Context, bucket string) error {
	if _, err := s.bucketReplication(ctx, bucket); err != nil {
		return nil // idempotent
	}
	res := fanOut(ctx, len(s.drives), func(ctx context.Context, i int) (struct{}, error) {
		return struct{}{}, s.drives[i].Delete(ctx, bucket, s.replicationPath(), true)
	})
	if countOK(res) < s.writeQuorum() {
		return ErrWriteQuorum
	}
	return nil
}

// joinPath is path.Join without importing "path" (which is already imported in
// set.go). Used only in this file to keep the import list minimal.
func joinPath(a, b string) string {
	return fmt.Sprintf("%s/%s", a, b)
}
