// SPDX-License-Identifier: Apache-2.0

// Package storage defines StorageAPI, the per-drive byte-oriented contract the
// object layer is built on, and the value types it exchanges (spec 2020, docs 05
// and 11.3).
//
// StorageAPI is deliberately narrow and free of object-layer concepts. It speaks
// in volumes (buckets), paths, byte ranges, and raw obj.meta bytes; it knows
// nothing about FileInfo, erasure math, or versioning. That decoupling is what
// lets one interface serve both a local drive (storage/local) and, later, a
// network drive over the wire: the remote backend marshals these same calls.
// Keeping obj.meta as opaque bytes here means the meta codec lives entirely in
// the object layer and storage never has to depend up into it.
package storage

import (
	"context"
	"errors"
	"io"
	"time"
)

// StorageAPI is the contract a single drive (local or remote) implements. All
// paths are slash-separated and relative to the volume root; implementations
// reject paths that escape the volume. Methods are safe for concurrent use.
type StorageAPI interface {
	// String returns a short stable identifier for the drive (its path or
	// endpoint), used in logs and errors.
	String() string

	// IsOnline reports whether the drive is currently reachable. A local drive is
	// online while its root is accessible.
	IsOnline() bool

	// MakeVol creates a volume (bucket directory). It returns ErrVolumeExists if
	// the volume already exists.
	MakeVol(ctx context.Context, volume string) error
	// StatVol returns information about a volume, or ErrVolumeNotFound.
	StatVol(ctx context.Context, volume string) (VolInfo, error)
	// ListVols returns all volumes on the drive.
	ListVols(ctx context.Context) ([]VolInfo, error)
	// DeleteVol removes a volume. If force is false it must be empty, else
	// ErrVolumeNotEmpty.
	DeleteVol(ctx context.Context, volume string, force bool) error

	// ReadMeta reads the raw obj.meta bytes for the object at path. The object
	// layer decodes them. Returns ErrFileNotFound if absent.
	ReadMeta(ctx context.Context, volume, path string) ([]byte, error)
	// WriteMeta writes raw obj.meta bytes atomically (write-temp-then-rename) for
	// the object at path, creating parent directories as needed.
	WriteMeta(ctx context.Context, volume, path string, data []byte) error

	// CreateFile streams exactly size bytes from r into the file at path. A size
	// of -1 streams until EOF. The write goes to a temp file and is fsync'd; the
	// caller commits it with RenameData.
	CreateFile(ctx context.Context, volume, path string, size int64, r io.Reader) error
	// ReadFile reads up to len(buf) bytes at offset from the file at path,
	// returning the number of bytes read.
	ReadFile(ctx context.Context, volume, path string, offset int64, buf []byte) (int, error)
	// ReadFileStream returns a reader over [offset, offset+length) of the file at
	// path. The caller must Close it. A length of -1 reads to end of file.
	ReadFileStream(ctx context.Context, volume, path string, offset, length int64) (io.ReadCloser, error)

	// RenameData atomically moves the data file and obj.meta of an object from a
	// staging path to its final path within the same volume. It is the commit
	// point of a write.
	RenameData(ctx context.Context, volume, srcPath, dstPath string) error
	// RenameFile atomically renames a single file within srcVolume/dstVolume.
	RenameFile(ctx context.Context, srcVolume, srcPath, dstVolume, dstPath string) error

	// Delete removes the file or (when recursive) directory tree at path.
	// Removing an absent path is not an error.
	Delete(ctx context.Context, volume, path string, recursive bool) error

	// StatFile returns metadata about a single file, or ErrFileNotFound.
	StatFile(ctx context.Context, volume, path string) (FileStat, error)
	// ListDir returns the immediate entries under path. Directory entries end in
	// "/". count < 0 returns all entries.
	ListDir(ctx context.Context, volume, path string, count int) ([]string, error)
}

// VolInfo describes a volume (bucket) on a drive.
type VolInfo struct {
	Name    string
	Created time.Time
}

// FileStat describes a single stored file.
type FileStat struct {
	Name    string
	Size    int64
	ModTime time.Time
	IsDir   bool
}

// DeleteOptions controls a Delete; reserved for future per-call tuning (for
// example skipping fsync of the parent directory on bulk deletes).
type DeleteOptions struct {
	Recursive bool
}

// Storage errors. The object layer maps these to S3 errors and uses them to
// drive quorum and healing decisions, so they are sentinel values compared with
// errors.Is.
var (
	ErrVolumeNotFound   = errors.New("storage: volume not found")
	ErrVolumeExists     = errors.New("storage: volume already exists")
	ErrVolumeNotEmpty   = errors.New("storage: volume not empty")
	ErrFileNotFound     = errors.New("storage: file not found")
	ErrFileAccessDenied = errors.New("storage: file access denied")
	ErrPathEscapes      = errors.New("storage: path escapes volume")
	ErrIsDirectory      = errors.New("storage: path is a directory")
	ErrShortWrite       = errors.New("storage: short write")
	ErrDriveOffline     = errors.New("storage: drive offline")
)
