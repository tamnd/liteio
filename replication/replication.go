// SPDX-License-Identifier: Apache-2.0

// Package replication implements S3-compatible bucket replication configuration
// and client logic for liteio (spec 2020, doc 09 §9.3). It supports one-way and
// active-active replication between liteio clusters with loop prevention.
package replication

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
)

// Status is the per-object replication state stored in FileInfo.Metadata.
type Status string

const (
	// StatusPending means the object is queued for replication to at least one rule.
	StatusPending Status = "PENDING"
	// StatusComplete means the object has been replicated to all matching rules.
	StatusComplete Status = "COMPLETE"
	// StatusFailed means replication failed and will not be automatically retried.
	StatusFailed Status = "FAILED"
	// StatusReplica marks an object that arrived via replication from a peer cluster.
	// Objects with REPLICA status are never re-replicated (loop prevention).
	StatusReplica Status = "REPLICA"
)

// Metadata keys stored in FileInfo.Metadata for replication tracking.
const (
	// MetaStatus is the object's replication status (Status constant).
	MetaStatus = "x-liteio-replication-status"
	// MetaSourceCluster identifies the source cluster for objects that arrived via
	// replication (present only when status is REPLICA).
	MetaSourceCluster = "x-liteio-replication-source-cluster"
)

// ReplicationConfig is the bucket replication configuration persisted on disk and
// exposed via PutBucketReplicationConfiguration.
type ReplicationConfig struct {
	Rules []Rule `json:"Rules,omitempty"`
}

// IsEmpty reports whether the config has no active rules.
func (rc ReplicationConfig) IsEmpty() bool { return len(rc.Rules) == 0 }

// Rule is one replication rule. Each rule identifies a destination and optional
// object filter. Multiple rules may match the same object; each is applied in
// order.
type Rule struct {
	// ID uniquely identifies this rule within the config. Used in status tracking.
	ID string `json:"ID,omitempty"`
	// Enabled, when false, suspends this rule without removing it.
	Enabled bool `json:"Enabled"`
	// Filter selects which objects this rule applies to.
	Filter Filter `json:"Filter,omitempty"`
	// Destination describes the target endpoint.
	Destination Destination `json:"Destination"`
	// DeleteReplication, when true, replicates delete markers and version deletions.
	DeleteReplication bool `json:"DeleteReplication,omitempty"`
	// ActiveActive, when true, marks this rule as part of an active-active pair.
	// Incoming replica objects (StatusReplica) skip this rule to prevent ping-pong.
	ActiveActive bool `json:"ActiveActive,omitempty"`
}

// Filter selects which objects a replication rule applies to.
type Filter struct {
	// Prefix restricts the rule to keys with this prefix. Empty matches all keys.
	Prefix string `json:"Prefix,omitempty"`
}

// Matches reports whether the filter applies to key.
func (f Filter) Matches(key string) bool {
	if f.Prefix == "" {
		return true
	}
	return strings.HasPrefix(key, f.Prefix)
}

// Destination is the replication target.
type Destination struct {
	// Endpoint is the base URL of the destination liteio (or S3-compatible) cluster.
	Endpoint string `json:"Endpoint"`
	// Bucket is the destination bucket. When empty the source bucket name is used.
	Bucket string `json:"Bucket,omitempty"`
	// Region is the AWS region of the destination (for SigV4 scope).
	Region string `json:"Region,omitempty"`
	// AccessKey and SecretKey are the credentials for the destination cluster.
	AccessKey string `json:"AccessKey"`
	SecretKey string `json:"SecretKey"`
}

// MatchingRules returns all enabled rules whose filter matches key and, when
// skipReplica is true, whose ActiveActive flag is false (replica objects skip
// active-active rules to prevent loops).
func (rc ReplicationConfig) MatchingRules(key string, skipReplica bool) []Rule {
	var out []Rule
	for _, r := range rc.Rules {
		if !r.Enabled {
			continue
		}
		if skipReplica && r.ActiveActive {
			continue // loop prevention: replica objects do not re-replicate
		}
		if r.Filter.Matches(key) {
			out = append(out, r)
		}
	}
	return out
}

// package-level replication counters. Incremented by the object layer after
// each replication attempt so callers can expose them via metrics.
var (
	cntReplicated atomic.Int64
	cntReplFailed atomic.Int64
)

// IncReplicated records one successful replication. Called by the object layer.
func IncReplicated() { cntReplicated.Add(1) }

// IncReplFailed records one failed replication attempt. Called by the object layer.
func IncReplFailed() { cntReplFailed.Add(1) }

// ReplicationStats returns the running totals for successful and failed
// replication operations since process start.
func ReplicationStats() (replicated int64, failed int64) {
	return cntReplicated.Load(), cntReplFailed.Load()
}

// ReplicaClient can replicate one object to a destination.
type ReplicaClient interface {
	// PutObject sends the object data to the destination bucket and key, with the
	// given user metadata. isReplica=true causes the destination to stamp the
	// object as REPLICA so it does not loop back.
	PutObject(ctx context.Context, bucket, key string, body io.Reader, size int64, userMeta map[string]string, isReplica bool) error
	// DeleteObject deletes the key (or versionID) from the destination.
	DeleteObject(ctx context.Context, bucket, key, versionID string) error
}
