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

// --- ListVersionsResult (GET /bucket?versions) ----------------------------

type listVersionsResult struct {
	XMLName             xml.Name          `xml:"ListVersionsResult"`
	XMLNS               string            `xml:"xmlns,attr"`
	Name                string            `xml:"Name"`
	Prefix              string            `xml:"Prefix"`
	KeyMarker           string            `xml:"KeyMarker"`
	VersionIDMarker     string            `xml:"VersionIdMarker"`
	NextKeyMarker       string            `xml:"NextKeyMarker,omitempty"`
	NextVersionIDMarker string            `xml:"NextVersionIdMarker,omitempty"`
	MaxKeys             int               `xml:"MaxKeys"`
	Delimiter           string            `xml:"Delimiter,omitempty"`
	IsTruncated         bool              `xml:"IsTruncated"`
	Versions            []versionEntryXML `xml:"Version"`
	DeleteMarkers       []deleteMarkerXML `xml:"DeleteMarker"`
	CommonPrefixes      []commonPrefix    `xml:"CommonPrefixes"`
}

type versionEntryXML struct {
	Key          string         `xml:"Key"`
	VersionID    string         `xml:"VersionId"`
	IsLatest     bool           `xml:"IsLatest"`
	LastModified string         `xml:"LastModified"`
	ETag         string         `xml:"ETag"`
	Size         int64          `xml:"Size"`
	StorageClass string         `xml:"StorageClass"`
	Owner        *canonicalUser `xml:"Owner,omitempty"`
}

type deleteMarkerXML struct {
	Key          string         `xml:"Key"`
	VersionID    string         `xml:"VersionId"`
	IsLatest     bool           `xml:"IsLatest"`
	LastModified string         `xml:"LastModified"`
	Owner        *canonicalUser `xml:"Owner,omitempty"`
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

// --- multipart upload ------------------------------------------------------

// InitiateMultipartUploadResult (POST /bucket/key?uploads).
type initiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	XMLNS    string   `xml:"xmlns,attr"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

// CompleteMultipartUpload request body (POST /bucket/key?uploadId=...).
type completeMultipartUpload struct {
	XMLName xml.Name          `xml:"CompleteMultipartUpload"`
	Parts   []completePartXML `xml:"Part"`
}

type completePartXML struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

// CompleteMultipartUploadResult response body.
type completeMultipartUploadResult struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	XMLNS    string   `xml:"xmlns,attr"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

// ListPartsResult (GET /bucket/key?uploadId=...).
type listPartsResult struct {
	XMLName              xml.Name      `xml:"ListPartsResult"`
	XMLNS                string        `xml:"xmlns,attr"`
	Bucket               string        `xml:"Bucket"`
	Key                  string        `xml:"Key"`
	UploadID             string        `xml:"UploadId"`
	PartNumberMarker     int           `xml:"PartNumberMarker"`
	NextPartNumberMarker int           `xml:"NextPartNumberMarker"`
	MaxParts             int           `xml:"MaxParts"`
	IsTruncated          bool          `xml:"IsTruncated"`
	StorageClass         string        `xml:"StorageClass"`
	Initiator            canonicalUser `xml:"Initiator"`
	Owner                canonicalUser `xml:"Owner"`
	Parts                []partXML     `xml:"Part"`
}

type partXML struct {
	PartNumber   int    `xml:"PartNumber"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
}

// ListMultipartUploadsResult (GET /bucket?uploads).
type listMultipartUploadsResult struct {
	XMLName            xml.Name       `xml:"ListMultipartUploadsResult"`
	XMLNS              string         `xml:"xmlns,attr"`
	Bucket             string         `xml:"Bucket"`
	KeyMarker          string         `xml:"KeyMarker"`
	UploadIDMarker     string         `xml:"UploadIdMarker"`
	NextKeyMarker      string         `xml:"NextKeyMarker"`
	NextUploadIDMarker string         `xml:"NextUploadIdMarker"`
	Delimiter          string         `xml:"Delimiter,omitempty"`
	Prefix             string         `xml:"Prefix"`
	MaxUploads         int            `xml:"MaxUploads"`
	IsTruncated        bool           `xml:"IsTruncated"`
	Uploads            []uploadXML    `xml:"Upload"`
	CommonPrefixes     []commonPrefix `xml:"CommonPrefixes"`
}

type uploadXML struct {
	Key          string        `xml:"Key"`
	UploadID     string        `xml:"UploadId"`
	Initiator    canonicalUser `xml:"Initiator"`
	Owner        canonicalUser `xml:"Owner"`
	StorageClass string        `xml:"StorageClass"`
	Initiated    string        `xml:"Initiated"`
}

// --- copy ------------------------------------------------------------------

// CopyObjectResult (PUT /bucket/key with x-amz-copy-source).
type copyObjectResult struct {
	XMLName      xml.Name `xml:"CopyObjectResult"`
	XMLNS        string   `xml:"xmlns,attr"`
	LastModified string   `xml:"LastModified"`
	ETag         string   `xml:"ETag"`
}

// --- tagging (PUT/GET/DELETE ?tagging on bucket and object) ---------------

// taggingRequest is the body of a PutObjectTagging / PutBucketTagging request.
type taggingRequest struct {
	XMLName xml.Name   `xml:"Tagging"`
	TagSet  []tagEntry `xml:"TagSet>Tag"`
}

// taggingResponse is the body of a GetObjectTagging / GetBucketTagging response.
type taggingResponse struct {
	XMLName xml.Name   `xml:"Tagging"`
	XMLNS   string     `xml:"xmlns,attr"`
	TagSet  []tagEntry `xml:"TagSet>Tag"`
}

type tagEntry struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

// tagsToXML converts a map to an ordered slice of tagEntry for XML rendering.
func tagsToXML(tags map[string]string) []tagEntry {
	entries := make([]tagEntry, 0, len(tags))
	for k, v := range tags {
		entries = append(entries, tagEntry{Key: k, Value: v})
	}
	return entries
}

// tagsFromXML converts the parsed tagEntry slice to a map. Duplicate keys keep
// the last value, matching S3's behavior.
func tagsFromXML(entries []tagEntry) map[string]string {
	m := make(map[string]string, len(entries))
	for _, e := range entries {
		m[e.Key] = e.Value
	}
	return m
}

// CopyPartResult (PUT /bucket/key?partNumber=N&uploadId=... with x-amz-copy-source).
type copyPartResult struct {
	XMLName      xml.Name `xml:"CopyPartResult"`
	XMLNS        string   `xml:"xmlns,attr"`
	LastModified string   `xml:"LastModified"`
	ETag         string   `xml:"ETag"`
}

// --- Object Lock XML types (GET/PUT /bucket?object-lock, /bucket/key?retention, /bucket/key?legal-hold) ---

type objectLockConfigurationXML struct {
	XMLName           xml.Name           `xml:"ObjectLockConfiguration"`
	XMLNS             string             `xml:"xmlns,attr,omitempty"`
	ObjectLockEnabled string             `xml:"ObjectLockEnabled"`
	Rule              *objectLockRuleXML `xml:"Rule,omitempty"`
}

type objectLockRuleXML struct {
	DefaultRetention defaultRetentionXML `xml:"DefaultRetention"`
}

type defaultRetentionXML struct {
	Mode  string `xml:"Mode"`
	Days  int    `xml:"Days,omitempty"`
	Years int    `xml:"Years,omitempty"`
}

type retentionXML struct {
	XMLName         xml.Name `xml:"Retention"`
	XMLNS           string   `xml:"xmlns,attr,omitempty"`
	Mode            string   `xml:"Mode"`
	RetainUntilDate string   `xml:"RetainUntilDate"`
}

type legalHoldXML struct {
	XMLName xml.Name `xml:"LegalHold"`
	XMLNS   string   `xml:"xmlns,attr,omitempty"`
	Status  string   `xml:"Status"`
}

// --- ListBucketResult (GET /bucket, ListObjects v1) --------------------------

type listBucketV1Result struct {
	XMLName        xml.Name            `xml:"ListBucketResult"`
	XMLNS          string              `xml:"xmlns,attr"`
	Name           string              `xml:"Name"`
	Prefix         string              `xml:"Prefix"`
	Marker         string              `xml:"Marker"`
	NextMarker     string              `xml:"NextMarker,omitempty"`
	MaxKeys        int                 `xml:"MaxKeys"`
	Delimiter      string              `xml:"Delimiter,omitempty"`
	IsTruncated    bool                `xml:"IsTruncated"`
	Contents       []objectEntry       `xml:"Contents"`
	CommonPrefixes []commonPrefixEntry `xml:"CommonPrefixes"`
}

type commonPrefixEntry struct {
	Prefix string `xml:"Prefix"`
}

// --- GetObjectAttributes (GET /bucket/key?attributes) -----------------------

type getObjectAttributesResponse struct {
	XMLName      xml.Name             `xml:"GetObjectAttributesResponse"`
	XMLNS        string               `xml:"xmlns,attr"`
	ETag         string               `xml:"ETag,omitempty"`
	StorageClass string               `xml:"StorageClass,omitempty"`
	ObjectSize   int64                `xml:"ObjectSize,omitempty"`
	ObjectParts  *objectPartsResponse `xml:"ObjectParts,omitempty"`
}

type objectPartsResponse struct {
	TotalPartsCount int              `xml:"TotalPartsCount"`
	Parts           []objectPartAttr `xml:"Part"`
}

type objectPartAttr struct {
	PartNumber    int    `xml:"PartNumber"`
	Size          int64  `xml:"Size"`
	ChecksumCRC32 string `xml:"ChecksumCRC32,omitempty"`
}
