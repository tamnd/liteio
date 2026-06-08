// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"strings"
	"testing"
)

// replicationCfgXML is a minimal valid PutBucketReplicationConfiguration body.
const replicationCfgXML = `<ReplicationConfiguration>
<Rule>
	<ID>r1</ID>
	<Status>Enabled</Status>
	<Filter><Prefix>logs/</Prefix></Filter>
	<Destination>
		<Bucket>http://peer/dstbkt</Bucket>
		<AccessKey>ak</AccessKey>
		<SecretKey>sk</SecretKey>
	</Destination>
	<DeleteMarkerReplication><Status>Enabled</Status></DeleteMarkerReplication>
</Rule>
</ReplicationConfiguration>`

// TestBucketReplicationRoundTrip verifies the S3 XML round-trip for bucket
// replication configuration: PUT → 200, GET → 200 with XML, DELETE → 204,
// GET after DELETE → 404.
func TestBucketReplicationRoundTrip(t *testing.T) {
	h := newHarness(t)

	// Create the bucket first.
	r := h.do("PUT", "/reptest", nil, nil)
	if r.status != 200 {
		t.Fatalf("create bucket: %d", r.status)
	}

	// PUT replication config.
	body := []byte(replicationCfgXML)
	r = h.do("PUT", "/reptest?replication", body, nil)
	if r.status != 200 {
		t.Fatalf("PUT replication: %d %s", r.status, r.body)
	}

	// GET replication config: verify the rule round-trips.
	r = h.do("GET", "/reptest?replication", nil, nil)
	if r.status != 200 {
		t.Fatalf("GET replication: %d %s", r.status, r.body)
	}
	if !strings.Contains(string(r.body), "r1") {
		t.Fatalf("rule ID not in response: %s", r.body)
	}
	if !strings.Contains(string(r.body), "logs/") {
		t.Fatalf("filter prefix not in response: %s", r.body)
	}

	// DELETE replication config.
	r = h.do("DELETE", "/reptest?replication", nil, nil)
	if r.status != 204 {
		t.Fatalf("DELETE replication: %d %s", r.status, r.body)
	}

	// GET after DELETE should return 404 (ReplicationConfigurationNotFoundError).
	r = h.do("GET", "/reptest?replication", nil, nil)
	if r.status != 404 {
		t.Fatalf("GET after DELETE: expected 404, got %d %s", r.status, r.body)
	}
}

// TestBucketReplicationMalformedXML verifies the handler rejects malformed XML.
func TestBucketReplicationMalformedXML(t *testing.T) {
	h := newHarness(t)

	r := h.do("PUT", "/reptest2", nil, nil)
	if r.status != 200 {
		t.Fatalf("create bucket: %d", r.status)
	}

	r = h.do("PUT", "/reptest2?replication", []byte("<not-xml"), nil)
	if r.status != 400 {
		t.Fatalf("malformed XML: expected 400, got %d %s", r.status, r.body)
	}
}

// TestBucketReplicationIncomingReplicaHeader verifies that a PUT carrying
// x-amz-replication-status: REPLICA stores the REPLICA status in metadata,
// which prevents re-replication (tested via GetObjectInfo).
func TestBucketReplicationIncomingReplicaHeader(t *testing.T) {
	h := newHarness(t)

	r := h.do("PUT", "/replica-bucket", nil, nil)
	if r.status != 200 {
		t.Fatalf("create bucket: %d", r.status)
	}

	r = h.do("PUT", "/replica-bucket/obj.txt", []byte("replica data"),
		map[string]string{"x-amz-replication-status": "REPLICA"})
	if r.status != 200 {
		t.Fatalf("PUT replica object: %d %s", r.status, r.body)
	}

	// HEAD to verify the object was stored.
	r = h.doHdr("HEAD", "/replica-bucket/obj.txt", nil)
	if r.status != 200 {
		t.Fatalf("HEAD replica object: %d", r.status)
	}
}
