// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"encoding/xml"
	"io"
	"net/http"

	"github.com/tamnd/liteio/replication"
)

// replicationConfigXML is the S3 wire shape for GetBucketReplicationConfiguration
// and PutBucketReplicationConfiguration.
type replicationConfigXML struct {
	XMLName xml.Name             `xml:"ReplicationConfiguration"`
	Rules   []replicationRuleXML `xml:"Rule"`
}

type replicationRuleXML struct {
	ID                      string               `xml:"ID,omitempty"`
	Status                  string               `xml:"Status"` // "Enabled" or "Disabled"
	Filter                  replicationFilterXML `xml:"Filter"`
	Destination             replicationDestXML   `xml:"Destination"`
	DeleteMarkerReplication replicationDeleteXML `xml:"DeleteMarkerReplication,omitempty"`
}

type replicationFilterXML struct {
	Prefix string `xml:"Prefix,omitempty"`
}

type replicationDestXML struct {
	// Bucket is the destination bucket ARN; liteio uses endpoint/bucket URL.
	Bucket    string `xml:"Bucket"`
	Region    string `xml:"StorageClass,omitempty"` // repurposed for region in liteio
	AccessKey string `xml:"AccessKey,omitempty"`
	SecretKey string `xml:"SecretKey,omitempty"`
}

type replicationDeleteXML struct {
	Status string `xml:"Status"` // "Enabled" or "Disabled"
}

// getBucketReplicationConfiguration handles GET /<bucket>?replication.
func (s *Server) getBucketReplicationConfiguration(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	cfg, err := s.layer.GetBucketReplication(r.Context(), bucket)
	if err != nil {
		writeError(w, requestID, r.URL.Path, toAPIError(err))
		return
	}
	out := toReplicationConfigXML(cfg)
	writeXML(w, requestID, http.StatusOK, out)
}

// putBucketReplicationConfiguration handles PUT /<bucket>?replication.
func (s *Server) putBucketReplicationConfiguration(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, requestID, r.URL.Path, errInvalidRequest)
		return
	}
	var x replicationConfigXML
	if err := xml.Unmarshal(body, &x); err != nil {
		writeError(w, requestID, r.URL.Path, errMalformedXML)
		return
	}
	cfg := fromReplicationConfigXML(x)
	if err := s.layer.SetBucketReplication(r.Context(), bucket, cfg); err != nil {
		writeError(w, requestID, r.URL.Path, toAPIError(err))
		return
	}
	w.WriteHeader(http.StatusOK)
}

// deleteBucketReplicationConfiguration handles DELETE /<bucket>?replication.
func (s *Server) deleteBucketReplicationConfiguration(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	if err := s.layer.DeleteBucketReplication(r.Context(), bucket); err != nil {
		writeError(w, requestID, r.URL.Path, toAPIError(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// isReplicationSource reports whether the request carries the incoming-replica
// marker. AWS SDK replication sends x-amz-replication-status: REPLICA on the
// object PUT; liteio uses this to record the object as a replica and prevent
// re-replication in active-active topologies.
func isReplicationSource(r *http.Request) bool {
	return r.Header.Get("x-amz-replication-status") == string(replication.StatusReplica)
}

// --- XML conversion helpers -------------------------------------------------

func toReplicationConfigXML(cfg replication.ReplicationConfig) replicationConfigXML {
	out := replicationConfigXML{}
	for _, r := range cfg.Rules {
		status := "Disabled"
		if r.Enabled {
			status = "Enabled"
		}
		delStatus := "Disabled"
		if r.DeleteReplication {
			delStatus = "Enabled"
		}
		out.Rules = append(out.Rules, replicationRuleXML{
			ID:     r.ID,
			Status: status,
			Filter: replicationFilterXML{Prefix: r.Filter.Prefix},
			Destination: replicationDestXML{
				Bucket:    r.Destination.Endpoint + "/" + r.Destination.Bucket,
				Region:    r.Destination.Region,
				AccessKey: r.Destination.AccessKey,
				SecretKey: r.Destination.SecretKey,
			},
			DeleteMarkerReplication: replicationDeleteXML{Status: delStatus},
		})
	}
	return out
}

func fromReplicationConfigXML(x replicationConfigXML) replication.ReplicationConfig {
	cfg := replication.ReplicationConfig{}
	for _, r := range x.Rules {
		dst := r.Destination
		// Bucket field may be "endpoint/bucket" or just a bucket name.
		endpoint, bucketName := dst.Bucket, ""
		for i := len(dst.Bucket) - 1; i >= 0; i-- {
			if dst.Bucket[i] == '/' {
				endpoint = dst.Bucket[:i]
				bucketName = dst.Bucket[i+1:]
				break
			}
		}
		cfg.Rules = append(cfg.Rules, replication.Rule{
			ID:                r.ID,
			Enabled:           r.Status == "Enabled",
			Filter:            replication.Filter{Prefix: r.Filter.Prefix},
			DeleteReplication: r.DeleteMarkerReplication.Status == "Enabled",
			Destination: replication.Destination{
				Endpoint:  endpoint,
				Bucket:    bucketName,
				Region:    dst.Region,
				AccessKey: dst.AccessKey,
				SecretKey: dst.SecretKey,
			},
		})
	}
	return cfg
}
