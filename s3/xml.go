// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"encoding/xml"
	"time"
)

// s3XMLNS is the namespace S3 stamps on its top-level response documents.
const s3XMLNS = "http://s3.amazonaws.com/doc/2006-03-01/"

// iso8601Millis is the timestamp format S3 uses in listing/version XML.
const iso8601Millis = "2006-01-02T15:04:05.000Z"

// amzTime formats t the way S3 renders timestamps in XML bodies.
func amzTime(t time.Time) string { return t.UTC().Format(iso8601Millis) }

// --- ListAllMyBuckets (GET /) ---------------------------------------------

type listAllMyBucketsResult struct {
	XMLName xml.Name      `xml:"ListAllMyBucketsResult"`
	XMLNS   string        `xml:"xmlns,attr"`
	Owner   canonicalUser `xml:"Owner"`
	Buckets bucketsList   `xml:"Buckets"`
}

type bucketsList struct {
	Bucket []bucketEntry `xml:"Bucket"`
}

type bucketEntry struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
}

type canonicalUser struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

// --- ListBucketResult (GET /bucket?list-type=2) ---------------------------

type listBucketV2Result struct {
	XMLName               xml.Name       `xml:"ListBucketResult"`
	XMLNS                 string         `xml:"xmlns,attr"`
	Name                  string         `xml:"Name"`
	Prefix                string         `xml:"Prefix"`
	StartAfter            string         `xml:"StartAfter,omitempty"`
	ContinuationToken     string         `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string         `xml:"NextContinuationToken,omitempty"`
	KeyCount              int            `xml:"KeyCount"`
	MaxKeys               int            `xml:"MaxKeys"`
	Delimiter             string         `xml:"Delimiter,omitempty"`
	IsTruncated           bool           `xml:"IsTruncated"`
	Contents              []objectEntry  `xml:"Contents"`
	CommonPrefixes        []commonPrefix `xml:"CommonPrefixes"`
}

type objectEntry struct {
	Key          string         `xml:"Key"`
	LastModified string         `xml:"LastModified"`
	ETag         string         `xml:"ETag"`
	Size         int64          `xml:"Size"`
	StorageClass string         `xml:"StorageClass"`
	Owner        *canonicalUser `xml:"Owner,omitempty"`
}

type commonPrefix struct {
	Prefix string `xml:"Prefix"`
}

// --- LocationConstraint (GET /bucket?location) ----------------------------

type locationConstraint struct {
	XMLName xml.Name `xml:"LocationConstraint"`
	XMLNS   string   `xml:"xmlns,attr"`
	Value   string   `xml:",chardata"`
}

// --- VersioningConfiguration (GET/PUT /bucket?versioning) -----------------

type versioningConfiguration struct {
	XMLName xml.Name `xml:"VersioningConfiguration"`
	XMLNS   string   `xml:"xmlns,attr,omitempty"`
	Status  string   `xml:"Status,omitempty"` // "Enabled" | "Suspended"
}

// --- Delete (POST /bucket?delete) -----------------------------------------

type deleteRequest struct {
	XMLName xml.Name             `xml:"Delete"`
	Quiet   bool                 `xml:"Quiet"`
	Objects []deleteRequestEntry `xml:"Object"`
}

type deleteRequestEntry struct {
	Key       string `xml:"Key"`
	VersionID string `xml:"VersionId"`
}

type deleteResult struct {
	XMLName xml.Name         `xml:"DeleteResult"`
	XMLNS   string           `xml:"xmlns,attr"`
	Deleted []deletedEntry   `xml:"Deleted"`
	Errors  []deleteErrorXML `xml:"Error"`
}

type deletedEntry struct {
	Key                   string `xml:"Key"`
	VersionID             string `xml:"VersionId,omitempty"`
	DeleteMarker          bool   `xml:"DeleteMarker,omitempty"`
	DeleteMarkerVersionID string `xml:"DeleteMarkerVersionId,omitempty"`
}

type deleteErrorXML struct {
	Key     string `xml:"Key"`
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}
