// SPDX-License-Identifier: Apache-2.0

package object

import (
	"context"
	"encoding/json"
	"path"

	"github.com/tamnd/liteio/event"
)

// SetBucketNotification implements ObjectLayer: stores the notification config
// as JSON under .liteio.sys/notification on every set.
func (sp *ServerPools) SetBucketNotification(ctx context.Context, bucket string, cfg event.NotificationConfig) error {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return err
	}
	doc, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	for _, set := range sp.allSets() {
		if err := set.setBucketNotification(ctx, bucket, doc); err != nil {
			return err
		}
	}
	return nil
}

// GetBucketNotification implements ObjectLayer: returns the notification config,
// or an empty config when none is set (notification config is optional).
func (sp *ServerPools) GetBucketNotification(ctx context.Context, bucket string) (event.NotificationConfig, error) {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return event.NotificationConfig{}, err
	}
	doc, err := sp.allSets()[0].bucketNotification(ctx, bucket)
	if err != nil {
		return event.NotificationConfig{}, nil // no config = empty (OK)
	}
	var cfg event.NotificationConfig
	if err := json.Unmarshal(doc, &cfg); err != nil {
		return event.NotificationConfig{}, nil
	}
	return cfg, nil
}

// DeleteBucketNotification implements ObjectLayer: removes the notification config.
// Idempotent.
func (sp *ServerPools) DeleteBucketNotification(ctx context.Context, bucket string) error {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return err
	}
	for _, set := range sp.allSets() {
		if err := set.deleteBucketNotification(ctx, bucket); err != nil {
			return err
		}
	}
	return nil
}

// notifyPut fires an ObjectCreated event for the given object info after a
// successful PUT (or CompleteMultipartUpload).
func (sp *ServerPools) notifyPut(ctx context.Context, oi ObjectInfo, name event.EventName, sourceIP string) {
	if sp.dispatcher == nil {
		return
	}
	cfg, err := sp.GetBucketNotification(ctx, oi.Bucket)
	if err != nil || len(cfg.WebhookConfigurations)+len(cfg.QueueConfigurations) == 0 {
		return
	}
	rec := event.Record{
		EventVersion:      "2.1",
		EventSource:       "liteio:s3",
		AwsRegion:         "us-east-1",
		EventTime:         sp.now(),
		EventName:         name,
		RequestParameters: event.RequestParameters{SourceIPAddress: sourceIP},
		S3: event.S3Entity{
			SchemaVersion: "1.0",
			Bucket:        event.BucketID{Name: oi.Bucket, ARN: "arn:aws:s3:::" + oi.Bucket},
			Object:        event.ObjectID{Key: oi.Name, Size: oi.Size, ETag: oi.ETag, VersionID: oi.VersionID},
		},
	}
	sp.dispatcher.Dispatch(cfg, name, oi.Name, []event.Record{rec})
}

// notifyDelete fires an ObjectRemoved event after a successful delete.
func (sp *ServerPools) notifyDelete(ctx context.Context, bucket, key, versionID string, isDeleteMarker bool, sourceIP string) {
	if sp.dispatcher == nil {
		return
	}
	cfg, err := sp.GetBucketNotification(ctx, bucket)
	if err != nil || len(cfg.WebhookConfigurations)+len(cfg.QueueConfigurations) == 0 {
		return
	}
	name := event.ObjectRemovedDelete
	if isDeleteMarker {
		name = event.ObjectRemovedDeleteMarkerCreated
	}
	rec := event.Record{
		EventVersion:      "2.1",
		EventSource:       "liteio:s3",
		AwsRegion:         "us-east-1",
		EventTime:         sp.now(),
		EventName:         name,
		RequestParameters: event.RequestParameters{SourceIPAddress: sourceIP},
		S3: event.S3Entity{
			SchemaVersion: "1.0",
			Bucket:        event.BucketID{Name: bucket, ARN: "arn:aws:s3:::" + bucket},
			Object:        event.ObjectID{Key: key, VersionID: versionID},
		},
	}
	sp.dispatcher.Dispatch(cfg, name, key, []event.Record{rec})
}

// --- erasureSet helpers --------------------------------------------------

func (s *erasureSet) notificationPath() string { return path.Join(reserved, "notification") }

func (s *erasureSet) setBucketNotification(ctx context.Context, bucket string, doc []byte) error {
	res := fanOut(ctx, len(s.drives), func(ctx context.Context, i int) (struct{}, error) {
		return struct{}{}, s.drives[i].WriteMeta(ctx, bucket, s.notificationPath(), doc)
	})
	if countOK(res) < s.writeQuorum() {
		return ErrWriteQuorum
	}
	return nil
}

func (s *erasureSet) bucketNotification(ctx context.Context, bucket string) ([]byte, error) {
	for _, d := range s.drives {
		if !d.IsOnline() {
			continue
		}
		data, err := d.ReadMeta(ctx, bucket, s.notificationPath())
		if err != nil {
			continue
		}
		return data, nil
	}
	return nil, ErrNoSuchBucketNotification
}

func (s *erasureSet) deleteBucketNotification(ctx context.Context, bucket string) error {
	if _, err := s.bucketNotification(ctx, bucket); err != nil {
		return nil // idempotent
	}
	res := fanOut(ctx, len(s.drives), func(ctx context.Context, i int) (struct{}, error) {
		return struct{}{}, s.drives[i].Delete(ctx, bucket, s.notificationPath(), true)
	})
	if countOK(res) < s.writeQuorum() {
		return ErrWriteQuorum
	}
	return nil
}
