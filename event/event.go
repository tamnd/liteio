// SPDX-License-Identifier: Apache-2.0

// Package event implements S3-compatible bucket notification: event record types,
// a notification configuration store, and an asynchronous dispatcher that delivers
// events to webhook targets (doc 09 §9.5). The shape of every event record matches
// the S3 event notification JSON so existing consumers work without change.
package event

import (
	"strings"
	"time"
)

// EventName identifies the S3 event that occurred.
type EventName string

// S3 event name constants matching the AWS event notification schema.
const (
	ObjectCreatedPut                     EventName = "s3:ObjectCreated:Put"
	ObjectCreatedPost                    EventName = "s3:ObjectCreated:Post"
	ObjectCreatedCopy                    EventName = "s3:ObjectCreated:Copy"
	ObjectCreatedCompleteMultipartUpload EventName = "s3:ObjectCreated:CompleteMultipartUpload"
	ObjectRemovedDelete                  EventName = "s3:ObjectRemoved:Delete"
	ObjectRemovedDeleteMarkerCreated     EventName = "s3:ObjectRemoved:DeleteMarkerCreated"

	// Restore events (spec 09 §9.5).
	ObjectRestoreInitiated EventName = "s3:ObjectRestore:Post"
	ObjectRestoreCompleted EventName = "s3:ObjectRestore:Completed"

	// Replication lifecycle events (spec 09 §9.5).
	ReplicationOperationFailed          EventName = "s3:Replication:OperationFailedReplication"
	ReplicationOperationMissedThreshold EventName = "s3:Replication:OperationMissedThreshold"
	ReplicationOperationNotTracked      EventName = "s3:Replication:OperationNotTracked"
)

// Record is the S3 event notification payload for one object event (doc 09 §9.5).
// The outer envelope used over the wire wraps a slice of these.
type Record struct {
	EventVersion      string            `json:"eventVersion"`
	EventSource       string            `json:"eventSource"`
	AwsRegion         string            `json:"awsRegion"`
	EventTime         time.Time         `json:"eventTime"`
	EventName         EventName         `json:"eventName"`
	RequestParameters RequestParameters `json:"requestParameters"`
	S3                S3Entity          `json:"s3"`
}

// RequestParameters carries per-request metadata attached to the event.
type RequestParameters struct {
	SourceIPAddress string `json:"sourceIPAddress,omitempty"`
}

// S3Entity is the embedded S3 context inside a Record.
type S3Entity struct {
	SchemaVersion   string   `json:"s3SchemaVersion"`
	ConfigurationID string   `json:"configurationId"`
	Bucket          BucketID `json:"bucket"`
	Object          ObjectID `json:"object"`
}

// BucketID is the bucket-identity block in an S3 event.
type BucketID struct {
	Name string `json:"name"`
	ARN  string `json:"arn"`
}

// ObjectID is the object-identity block in an S3 event.
type ObjectID struct {
	Key       string `json:"key"`
	Size      int64  `json:"size,omitempty"`
	ETag      string `json:"eTag,omitempty"`
	VersionID string `json:"versionId,omitempty"`
	Sequencer string `json:"sequencer,omitempty"`
}

// Envelope is the outer wrapper that S3 posts to a webhook — a JSON object with
// a single "Records" key containing a list of Record values.
type Envelope struct {
	Records []Record `json:"Records"`
}

// NotificationConfig is the bucket notification configuration
// (PutBucketNotificationConfiguration body). Only the QueueConfiguration,
// LambdaFunctionConfiguration, and TopicConfiguration sibling keys are part of
// the full S3 spec; liteio ships webhook targets for M7 and can extend later.
type NotificationConfig struct {
	// WebhookConfigurations holds the liteio-native webhook targets. The S3 API
	// uses QueueConfiguration/TopicConfiguration/LambdaFunctionConfiguration; we
	// store a superset and expose those fields for S3 compatibility while also
	// accepting a native webhook target list via a separate field.
	QueueConfigurations   []QueueConfig   `json:"QueueConfigurations,omitempty"`
	WebhookConfigurations []WebhookConfig `json:"WebhookConfigurations,omitempty"`
}

// QueueConfig is an SQS-style notification rule. liteio's webhook target is
// addressed by ARN (an HTTP/HTTPS URL is accepted as an informal ARN).
type QueueConfig struct {
	ID       string      `json:"Id,omitempty"`
	QueueARN string      `json:"Queue"`
	Events   []EventName `json:"Events"`
	Filter   *Filter     `json:"Filter,omitempty"`
}

// WebhookConfig is a liteio-native notification target: an HTTP/HTTPS URL that
// receives S3 event JSON payloads via POST.
type WebhookConfig struct {
	ID     string      `json:"Id,omitempty"`
	URL    string      `json:"URL"`
	Events []EventName `json:"Events"`
	Filter *Filter     `json:"Filter,omitempty"`
}

// Filter selects which keys a rule applies to.
type Filter struct {
	Key KeyFilter `json:"Key,omitempty"`
}

// KeyFilter is the S3 prefix/suffix filter shape.
type KeyFilter struct {
	FilterRules []FilterRule `json:"FilterRules,omitempty"`
}

// FilterRule is one prefix or suffix rule.
type FilterRule struct {
	Name  string `json:"Name"` // "Prefix" or "Suffix"
	Value string `json:"Value"`
}

// Matches reports whether key satisfies all of r's filter rules (empty filter
// matches every key).
func (r *FilterRule) matches(key string) bool {
	switch strings.ToLower(r.Name) {
	case "prefix":
		return strings.HasPrefix(key, r.Value)
	case "suffix":
		return strings.HasSuffix(key, r.Value)
	}
	return true
}

// filterMatches reports whether the filter passes for key.
func filterMatches(f *Filter, key string) bool {
	if f == nil {
		return true
	}
	for i := range f.Key.FilterRules {
		if !f.Key.FilterRules[i].matches(key) {
			return false
		}
	}
	return true
}

// eventMatches reports whether name appears in the events list of a rule.
// Rules may use a wildcard suffix (s3:ObjectCreated:* matches any created event).
func eventMatches(events []EventName, name EventName) bool {
	for _, e := range events {
		if e == name {
			return true
		}
		// Wildcard suffix: s3:ObjectCreated:* → matches s3:ObjectCreated:Put etc.
		if strings.HasSuffix(string(e), ":*") {
			prefix := strings.TrimSuffix(string(e), "*")
			if strings.HasPrefix(string(name), prefix) {
				return true
			}
		}
	}
	return false
}

// MatchingQueueIDs returns the IDs of all QueueConfigurations whose QueueARN is
// not an HTTP/HTTPS URL (i.e. named queue targets), and whose event and key
// filter match the given event name and object key.
func MatchingQueueIDs(cfg NotificationConfig, name EventName, key string) []string {
	var out []string
	for _, q := range cfg.QueueConfigurations {
		if strings.HasPrefix(q.QueueARN, "http://") || strings.HasPrefix(q.QueueARN, "https://") {
			continue
		}
		if eventMatches(q.Events, name) && filterMatches(q.Filter, key) {
			id := q.ID
			if id == "" {
				id = q.QueueARN
			}
			out = append(out, id)
		}
	}
	return out
}

// MatchingWebhooks returns all webhook targets from cfg whose event and key
// filter match the given event name and object key.
func MatchingWebhooks(cfg NotificationConfig, name EventName, key string) []WebhookConfig {
	var out []WebhookConfig
	for _, wh := range cfg.WebhookConfigurations {
		if eventMatches(wh.Events, name) && filterMatches(wh.Filter, key) {
			out = append(out, wh)
		}
	}
	// Treat QueueConfiguration URLs (if URL-shaped) as webhooks.
	for _, q := range cfg.QueueConfigurations {
		if !strings.HasPrefix(q.QueueARN, "http://") && !strings.HasPrefix(q.QueueARN, "https://") {
			continue
		}
		if eventMatches(q.Events, name) && filterMatches(q.Filter, q.QueueARN) {
			out = append(out, WebhookConfig{
				ID:     q.ID,
				URL:    q.QueueARN,
				Events: q.Events,
				Filter: q.Filter,
			})
		}
	}
	return out
}
