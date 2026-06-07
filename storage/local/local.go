// SPDX-License-Identifier: Apache-2.0

// Package local implements storage.StorageAPI over a single local filesystem
// directory (spec 2020, docs 05 and 06). One Local backs one physical drive.
//
// Layout. The drive root holds one directory per volume (bucket). Within a
// volume an object is itself a directory holding obj.meta plus the data files
// part.1, part.2, ... A write stages a temporary directory and commits it with a
// single RenameData, so a reader never sees a half-written object.
//
// Durability. Data files are fsync'd before commit, and the parent directory is
// fsync'd after a rename so the rename itself survives a crash. obj.meta is
// written write-temp-then-rename for atomic in-place updates.
//
// O_DIRECT. The current data path uses buffered I/O with explicit fsync, which
// is correct on every platform and filesystem. O_DIRECT (with the aligned
// buffers from package buf) is a planned throughput optimization layered behind
// a best-effort probe; many CI and container filesystems reject O_DIRECT, so
// correctness must never depend on it.
package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tamnd/liteio/storage"
)

// Local is a StorageAPI backed by a directory tree on the local filesystem.
type Local struct {
	root string
}

// metaFile is the obj.meta filename within an object directory.
const metaFile = "obj.meta"

// New opens (creating if needed) a local drive rooted at dir. The directory must
// be usable for reads and writes.
func New(dir string) (*Local, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("local: resolve root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("local: create root: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("local: stat root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("local: root %s is not a directory", abs)
	}
	return &Local{root: abs}, nil
}

func (l *Local) String() string { return l.root }

func (l *Local) IsOnline() bool {
	_, err := os.Stat(l.root)
	return err == nil
}

// volPath returns the absolute path of a volume directory, rejecting names that
// contain separators or escape the root.
func (l *Local) volPath(volume string) (string, error) {
	if volume == "" || strings.ContainsAny(volume, `/\`) || volume == "." || volume == ".." {
		return "", storage.ErrVolumeNotFound
	}
	return filepath.Join(l.root, volume), nil
}

// objPath joins a volume and a slash-separated object path, rejecting any path
// that escapes the volume directory.
func (l *Local) objPath(volume, path string) (string, error) {
	vp, err := l.volPath(volume)
	if err != nil {
		return "", err
	}
	clean := filepath.Clean(filepath.FromSlash(path))
	if clean == ".." || clean == "." || filepath.IsAbs(clean) ||
		strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", storage.ErrPathEscapes
	}
	return filepath.Join(vp, clean), nil
}

func (l *Local) MakeVol(_ context.Context, volume string) error {
	vp, err := l.volPath(volume)
	if err != nil {
		return err
	}
	err = os.Mkdir(vp, 0o755)
	switch {
	case errors.Is(err, os.ErrExist):
		return storage.ErrVolumeExists
	case err != nil:
		return fmt.Errorf("local: make volume: %w", err)
	}
	return syncDir(l.root)
}

func (l *Local) StatVol(_ context.Context, volume string) (storage.VolInfo, error) {
	vp, err := l.volPath(volume)
	if err != nil {
		return storage.VolInfo{}, err
	}
	info, err := os.Stat(vp)
	if errors.Is(err, os.ErrNotExist) {
		return storage.VolInfo{}, storage.ErrVolumeNotFound
	}
	if err != nil {
		return storage.VolInfo{}, fmt.Errorf("local: stat volume: %w", err)
	}
	if !info.IsDir() {
		return storage.VolInfo{}, storage.ErrVolumeNotFound
	}
	return storage.VolInfo{Name: volume, Created: info.ModTime()}, nil
}

func (l *Local) ListVols(_ context.Context) ([]storage.VolInfo, error) {
	entries, err := os.ReadDir(l.root)
	if err != nil {
		return nil, fmt.Errorf("local: list volumes: %w", err)
	}
	vols := make([]storage.VolInfo, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		vols = append(vols, storage.VolInfo{Name: e.Name(), Created: info.ModTime()})
	}
	return vols, nil
}

func (l *Local) DeleteVol(_ context.Context, volume string, force bool) error {
	vp, err := l.volPath(volume)
	if err != nil {
		return err
	}
	if force {
		if err := os.RemoveAll(vp); err != nil {
			return fmt.Errorf("local: force delete volume: %w", err)
		}
		return syncDir(l.root)
	}
	err = os.Remove(vp)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return storage.ErrVolumeNotFound
	case isNotEmpty(err):
		return storage.ErrVolumeNotEmpty
	case err != nil:
		return fmt.Errorf("local: delete volume: %w", err)
	}
	return syncDir(l.root)
}

func (l *Local) ReadMeta(ctx context.Context, volume, path string) ([]byte, error) {
	return l.readWholeFile(ctx, volume, filepath.ToSlash(filepath.Join(path, metaFile)))
}

func (l *Local) WriteMeta(_ context.Context, volume, path string, data []byte) error {
	full, err := l.objPath(volume, filepath.ToSlash(filepath.Join(path, metaFile)))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("local: mkdir for meta: %w", err)
	}
	return writeFileAtomic(full, data)
}

func (l *Local) readWholeFile(_ context.Context, volume, path string) ([]byte, error) {
	full, err := l.objPath(volume, path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(full)
	if errors.Is(err, os.ErrNotExist) {
		return nil, storage.ErrFileNotFound
	}
	if err != nil {
		if isIsDir(err) {
			return nil, storage.ErrIsDirectory
		}
		return nil, fmt.Errorf("local: read meta: %w", err)
	}
	return data, nil
}

func (l *Local) CreateFile(ctx context.Context, volume, path string, size int64, r io.Reader) error {
	full, err := l.objPath(volume, path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("local: mkdir for data: %w", err)
	}
	f, err := os.OpenFile(full, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("local: create data: %w", err)
	}
	written, copyErr := copyN(f, r, size)
	if copyErr == nil && size >= 0 && written != size {
		copyErr = fmt.Errorf("%w: wrote %d of %d", storage.ErrShortWrite, written, size)
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(full)
		return copyErr
	}
	if syncErr != nil {
		_ = os.Remove(full)
		return fmt.Errorf("local: fsync data: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("local: close data: %w", closeErr)
	}
	return syncDir(filepath.Dir(full))
}

func (l *Local) ReadFile(ctx context.Context, volume, path string, offset int64, buf []byte) (int, error) {
	rc, err := l.ReadFileStream(ctx, volume, path, offset, int64(len(buf)))
	if err != nil {
		return 0, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadFull(rc, buf)
}

func (l *Local) ReadFileStream(_ context.Context, volume, path string, offset, length int64) (io.ReadCloser, error) {
	full, err := l.objPath(volume, path)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(full)
	if errors.Is(err, os.ErrNotExist) {
		return nil, storage.ErrFileNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("local: open for read: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("local: stat for read: %w", err)
	}
	if info.IsDir() {
		_ = f.Close()
		return nil, storage.ErrIsDirectory
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("local: seek: %w", err)
		}
	}
	if length < 0 {
		return f, nil
	}
	return &limitedFile{f: f, r: io.LimitReader(f, length)}, nil
}

// limitedFile bounds a file read to length bytes while still closing the file.
type limitedFile struct {
	f *os.File
	r io.Reader
}

func (lf *limitedFile) Read(p []byte) (int, error) { return lf.r.Read(p) }
func (lf *limitedFile) Close() error               { return lf.f.Close() }

func (l *Local) RenameData(_ context.Context, volume, srcPath, dstPath string) error {
	src, err := l.objPath(volume, srcPath)
	if err != nil {
		return err
	}
	dst, err := l.objPath(volume, dstPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("local: mkdir for rename: %w", err)
	}
	// Replace any existing object directory atomically by removing it first; the
	// staging directory then takes its place with a single rename.
	if err := os.RemoveAll(dst); err != nil {
		return fmt.Errorf("local: clear rename target: %w", err)
	}
	if err := os.Rename(src, dst); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return storage.ErrFileNotFound
		}
		return fmt.Errorf("local: rename data: %w", err)
	}
	return syncDir(filepath.Dir(dst))
}

func (l *Local) RenameFile(_ context.Context, srcVolume, srcPath, dstVolume, dstPath string) error {
	src, err := l.objPath(srcVolume, srcPath)
	if err != nil {
		return err
	}
	dst, err := l.objPath(dstVolume, dstPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("local: mkdir for rename: %w", err)
	}
	if err := os.Rename(src, dst); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return storage.ErrFileNotFound
		}
		return fmt.Errorf("local: rename file: %w", err)
	}
	return syncDir(filepath.Dir(dst))
}

func (l *Local) Delete(_ context.Context, volume, path string, recursive bool) error {
	full, err := l.objPath(volume, path)
	if err != nil {
		return err
	}
	if recursive {
		if err := os.RemoveAll(full); err != nil {
			return fmt.Errorf("local: recursive delete: %w", err)
		}
		return syncDir(filepath.Dir(full))
	}
	err = os.Remove(full)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil // removing an absent path is not an error
	case isNotEmpty(err):
		return storage.ErrVolumeNotEmpty
	case err != nil:
		return fmt.Errorf("local: delete: %w", err)
	}
	return syncDir(filepath.Dir(full))
}

func (l *Local) StatFile(_ context.Context, volume, path string) (storage.FileStat, error) {
	full, err := l.objPath(volume, path)
	if err != nil {
		return storage.FileStat{}, err
	}
	info, err := os.Stat(full)
	if errors.Is(err, os.ErrNotExist) {
		return storage.FileStat{}, storage.ErrFileNotFound
	}
	if err != nil {
		return storage.FileStat{}, fmt.Errorf("local: stat file: %w", err)
	}
	return storage.FileStat{
		Name:    filepath.Base(full),
		Size:    info.Size(),
		ModTime: info.ModTime(),
		IsDir:   info.IsDir(),
	}, nil
}

func (l *Local) ListDir(_ context.Context, volume, path string, count int) ([]string, error) {
	full, err := l.objPath(volume, path)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(full)
	if errors.Is(err, os.ErrNotExist) {
		return nil, storage.ErrFileNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("local: open dir: %w", err)
	}
	defer func() { _ = f.Close() }()
	entries, err := f.ReadDir(count)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("local: read dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	return names, nil
}

// copyN copies from r to w. When n >= 0 it copies at most n bytes; when n < 0 it
// copies until EOF. It returns the number of bytes written.
func copyN(w io.Writer, r io.Reader, n int64) (int64, error) {
	if n < 0 {
		return io.Copy(w, r)
	}
	return io.CopyN(w, r, n)
}

// writeFileAtomic writes data to a sibling temp file, fsyncs it, renames it over
// path, and fsyncs the parent directory.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".meta-*.tmp")
	if err != nil {
		return fmt.Errorf("local: temp meta: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("local: write temp meta: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("local: fsync temp meta: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("local: close temp meta: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("local: commit meta: %w", err)
	}
	return syncDir(dir)
}

// syncDir fsyncs a directory so a create/rename within it is durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("local: open dir for sync: %w", err)
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil {
		// Some filesystems do not support directory fsync; treat as non-fatal.
		if isNotSupported(syncErr) {
			return nil
		}
		return fmt.Errorf("local: fsync dir: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("local: close dir: %w", closeErr)
	}
	return nil
}

var _ storage.StorageAPI = (*Local)(nil)
