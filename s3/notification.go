// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"encoding/xml"
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/tamnd/liteio/event"
	"github.com/tamnd/liteio/object"
)

// getBucketNotificationConfiguration handles GET /bucket?notification.
func (s *Server) getBucketNotificationConfiguration(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	cfg, err := s.layer.GetBucketNotification(r.Context(), bucket)
	if err != nil {
		if errors.Is(err, object.ErrBucketNotFound) {
			writeError(w, requestID, r.URL.Path, errNoSuchBucket)
			return
		}
		writeError(w, requestID, r.URL.Path, toAPIError(err))
		return
	}
	// Encode as S3 NotificationConfiguration XML.
	out := notificationConfigToXML(cfg)
	b, _ := xml.Marshal(out)
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	w.Write(append([]byte(xml.Header), b...)) //nolint:errcheck
}

// putBucketNotificationConfiguration handles PUT /bucket?notification.
func (s *Server) putBucketNotificationConfiguration(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	if _, err := s.layer.GetBucketInfo(r.Context(), bucket); err != nil {
		if errors.Is(err, object.ErrBucketNotFound) {
			writeError(w, requestID, r.URL.Path, errNoSuchBucket)
			return
		}
		writeError(w, requestID, r.URL.Path, toAPIError(err))
		return
	}
	var x notificationConfigurationXML
	dec := xml.NewDecoder(r.Body)
	if err := dec.Decode(&x); err != nil {
		writeError(w, requestID, r.URL.Path, errMalformedXML)
		return
	}
	cfg := notificationConfigFromXML(x)
	if err := s.layer.SetBucketNotification(r.Context(), bucket, cfg); err != nil {
		writeError(w, requestID, r.URL.Path, toAPIError(err))
		return
	}
	w.WriteHeader(http.StatusOK)
}

// deleteNotificationConfiguration handles DELETE /bucket?notification.
func (s *Server) deleteNotificationConfiguration(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	if err := s.layer.DeleteBucketNotification(r.Context(), bucket); err != nil {
		if errors.Is(err, object.ErrBucketNotFound) {
			writeError(w, requestID, r.URL.Path, errNoSuchBucket)
			return
		}
		writeError(w, requestID, r.URL.Path, toAPIError(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- XML shape ----------------------------------------------------------

type notificationConfigurationXML struct {
	XMLName             xml.Name         `xml:"NotificationConfiguration"`
	XMLNS               string           `xml:"xmlns,attr,omitempty"`
	QueueConfigurations []queueConfigXML `xml:"QueueConfiguration"`
}

type queueConfigXML struct {
	ID     string     `xml:"Id,omitempty"`
	Queue  string     `xml:"Queue"`
	Events []string   `xml:"Event"`
	Filter *filterXML `xml:"Filter,omitempty"`
}

type filterXML struct {
	Key keyFilterXML `xml:"S3Key"`
}

type keyFilterXML struct {
	FilterRules []filterRuleXML `xml:"FilterRule"`
}

type filterRuleXML struct {
	Name  string `xml:"Name"`
	Value string `xml:"Value"`
}

func notificationConfigToXML(cfg event.NotificationConfig) notificationConfigurationXML {
	var x notificationConfigurationXML
	x.XMLNS = "http://s3.amazonaws.com/doc/2006-03-01/"
	for _, q := range cfg.QueueConfigurations {
		qx := queueConfigXML{ID: q.ID, Queue: q.QueueARN}
		for _, e := range q.Events {
			qx.Events = append(qx.Events, string(e))
		}
		if q.Filter != nil {
			fx := &filterXML{}
			for _, r := range q.Filter.Key.FilterRules {
				fx.Key.FilterRules = append(fx.Key.FilterRules, filterRuleXML{Name: r.Name, Value: r.Value})
			}
			qx.Filter = fx
		}
		x.QueueConfigurations = append(x.QueueConfigurations, qx)
	}
	// Webhook configs are stored as JSON-native; expose them as QueueConfiguration
	// entries with their URL as the Queue ARN so standard S3 clients can round-trip.
	for _, wh := range cfg.WebhookConfigurations {
		qx := queueConfigXML{ID: wh.ID, Queue: wh.URL}
		for _, e := range wh.Events {
			qx.Events = append(qx.Events, string(e))
		}
		if wh.Filter != nil {
			fx := &filterXML{}
			for _, r := range wh.Filter.Key.FilterRules {
				fx.Key.FilterRules = append(fx.Key.FilterRules, filterRuleXML{Name: r.Name, Value: r.Value})
			}
			qx.Filter = fx
		}
		x.QueueConfigurations = append(x.QueueConfigurations, qx)
	}
	return x
}

func notificationConfigFromXML(x notificationConfigurationXML) event.NotificationConfig {
	var cfg event.NotificationConfig
	for _, q := range x.QueueConfigurations {
		qc := event.QueueConfig{ID: q.ID, QueueARN: q.Queue}
		for _, e := range q.Events {
			qc.Events = append(qc.Events, event.EventName(e))
		}
		if q.Filter != nil {
			f := &event.Filter{}
			for _, r := range q.Filter.Key.FilterRules {
				f.Key.FilterRules = append(f.Key.FilterRules, event.FilterRule{Name: r.Name, Value: r.Value})
			}
			qc.Filter = f
		}
		cfg.QueueConfigurations = append(cfg.QueueConfigurations, qc)
	}
	return cfg
}

// sourceIPFromRequest strips the port from a RemoteAddr for use in event records.
func sourceIPFromRequest(r *http.Request) string {
	addr := r.RemoteAddr
	if h := r.Header.Get("X-Forwarded-For"); h != "" {
		// X-Forwarded-For may be a comma-list; take the first entry.
		addr = strings.TrimSpace(strings.SplitN(h, ",", 2)[0])
	} else if h := r.Header.Get("X-Real-Ip"); h != "" {
		addr = h
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}
