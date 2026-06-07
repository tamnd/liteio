// SPDX-License-Identifier: Apache-2.0

package auth

import "sort"

// Canned policies are the built-in, attach-by-name policy documents liteio ships
// so operators have working defaults out of the box (spec 2020, doc 08.2). The
// names and grants match the common MinIO/AWS set, so muscle memory and existing
// automation carry over.
//
// They are stored as their JSON source and parsed on lookup; the set is small and
// lookups are not on a hot path. cannedPolicyDocs is the source of truth.
var cannedPolicyDocs = map[string]string{
	// readwrite: full S3 access to every bucket and object.
	"readwrite": `{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Action": ["s3:*"],
			"Resource": ["arn:aws:s3:::*"]
		}]
	}`,

	// readonly: read objects and locate buckets, nothing else.
	"readonly": `{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Action": ["s3:GetBucketLocation", "s3:GetObject", "s3:ListBucket"],
			"Resource": ["arn:aws:s3:::*"]
		}]
	}`,

	// writeonly: upload objects, no read or list (a drop box).
	"writeonly": `{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Action": ["s3:PutObject"],
			"Resource": ["arn:aws:s3:::*"]
		}]
	}`,

	// diagnostics: the read-only admin actions used for health and profiling, no
	// data-plane access.
	"diagnostics": `{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Action": [
				"admin:ServerInfo",
				"admin:Prometheus",
				"admin:Profiling",
				"admin:ServerTrace",
				"admin:ConsoleLog",
				"admin:TopLocksInfo",
				"admin:HealthInfo",
				"admin:BandwidthMonitor"
			],
			"Resource": ["arn:aws:s3:::*"]
		}]
	}`,

	// consoleAdmin: everything — data plane, admin plane, and KMS.
	"consoleAdmin": `{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Action": ["admin:*", "kms:*", "s3:*"],
			"Resource": ["arn:aws:s3:::*"]
		}]
	}`,
}

// CannedPolicy returns the named built-in policy and whether it exists.
func CannedPolicy(name string) (Policy, bool) {
	doc, ok := cannedPolicyDocs[name]
	if !ok {
		return Policy{}, false
	}
	p, err := ParsePolicy([]byte(doc))
	if err != nil {
		// A built-in document failing to parse is a programming error, caught by
		// TestCannedPoliciesParse; never reachable in a released binary.
		panic("auth: invalid canned policy " + name + ": " + err.Error())
	}
	return p, true
}

// CannedPolicyNames returns the built-in policy names in sorted order.
func CannedPolicyNames() []string {
	names := make([]string, 0, len(cannedPolicyDocs))
	for name := range cannedPolicyDocs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
