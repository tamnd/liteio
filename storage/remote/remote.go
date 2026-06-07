// SPDX-License-Identifier: Apache-2.0

// Package remote implements storage.StorageAPI over the inter-node transport in
// cluster/rpc (spec 2020, docs 06.7, 11.3, 11.9). A remote.Storage is a drive that
// lives one RPC hop away: every method marshals its call to the owning node, which
// runs it against its own local drive via the handlers Register installs. Because
// it satisfies the same StorageAPI as storage/local, the erasure set and placement
// layers above cannot tell a local drive from a remote one — the property that
// makes single-node and distributed liteio the same code (doc 01).
package remote

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tamnd/liteio/cluster/rpc"
	"github.com/tamnd/liteio/storage"
	"github.com/vmihailenco/msgpack/v5"
)

// RPC method names. They are part of the wire contract between peers and must
// stay stable across versions.
const (
	mIsOnline       = "IsOnline"
	mMakeVol        = "MakeVol"
	mStatVol        = "StatVol"
	mListVols       = "ListVols"
	mDeleteVol      = "DeleteVol"
	mReadMeta       = "ReadMeta"
	mWriteMeta      = "WriteMeta"
	mCreateFile     = "CreateFile"
	mReadFile       = "ReadFile"
	mReadFileStream = "ReadFileStream"
	mRenameData     = "RenameData"
	mRenameFile     = "RenameFile"
	mDelete         = "Delete"
	mStatFile       = "StatFile"
	mListDir        = "ListDir"
)

// onlineTimeout bounds the health probe IsOnline makes, so a dead peer reports
// offline promptly rather than blocking the caller.
const onlineTimeout = 2 * time.Second

// --- error codec -----------------------------------------------------------

// storageErrors are the sentinel errors that must survive a round trip so the
// object layer's errors.Is checks keep working across the network.
var storageErrors = []struct {
	code string
	err  error
}{
	{"VolumeNotFound", storage.ErrVolumeNotFound},
	{"VolumeExists", storage.ErrVolumeExists},
	{"VolumeNotEmpty", storage.ErrVolumeNotEmpty},
	{"FileNotFound", storage.ErrFileNotFound},
	{"FileAccessDenied", storage.ErrFileAccessDenied},
	{"PathEscapes", storage.ErrPathEscapes},
	{"IsDirectory", storage.ErrIsDirectory},
	{"ShortWrite", storage.ErrShortWrite},
	{"DriveOffline", storage.ErrDriveOffline},
	// io sentinels ReadFile surfaces via io.ReadFull, kept whole so callers'
	// errors.Is(err, io.EOF) checks still hold across the network.
	{"EOF", io.EOF},
	{"UnexpectedEOF", io.ErrUnexpectedEOF},
}

// RemoteError is the fallback for an error the peer raised that is not one of the
// known storage sentinels; it carries the peer's message.
type RemoteError struct{ Message string }

func (e *RemoteError) Error() string { return "remote: " + e.Message }

// encodeErr maps a handler error to its wire code: a known sentinel to its short
// name, anything else to an opaque message prefixed "x:".
func encodeErr(err error) string {
	if err == nil {
		return ""
	}
	for _, se := range storageErrors {
		if errors.Is(err, se.err) {
			return se.code
		}
	}
	return "x:" + err.Error()
}

// decodeErr is the inverse: a known code to its sentinel, an "x:"-prefixed code to
// a RemoteError.
func decodeErr(code string) error {
	for _, se := range storageErrors {
		if se.code == code {
			return se.err
		}
	}
	if msg, ok := strings.CutPrefix(code, "x:"); ok {
		return &RemoteError{Message: msg}
	}
	return &RemoteError{Message: code}
}

// --- argument and result types ---------------------------------------------

type volArgs struct {
	Volume string `msgpack:"v"`
}
type deleteVolArgs struct {
	Volume string `msgpack:"v"`
	Force  bool   `msgpack:"f"`
}
type pathArgs struct {
	Volume string `msgpack:"v"`
	Path   string `msgpack:"p"`
}
type writeMetaArgs struct {
	Volume string `msgpack:"v"`
	Path   string `msgpack:"p"`
	Data   []byte `msgpack:"d"`
}
type createFileArgs struct {
	Volume string `msgpack:"v"`
	Path   string `msgpack:"p"`
	Size   int64  `msgpack:"s"`
}
type readFileArgs struct {
	Volume string `msgpack:"v"`
	Path   string `msgpack:"p"`
	Offset int64  `msgpack:"o"`
	Length int64  `msgpack:"n"`
}
type renameDataArgs struct {
	Volume  string `msgpack:"v"`
	SrcPath string `msgpack:"s"`
	DstPath string `msgpack:"d"`
}
type renameFileArgs struct {
	SrcVolume string `msgpack:"sv"`
	SrcPath   string `msgpack:"sp"`
	DstVolume string `msgpack:"dv"`
	DstPath   string `msgpack:"dp"`
}
type deleteArgs struct {
	Volume    string `msgpack:"v"`
	Path      string `msgpack:"p"`
	Recursive bool   `msgpack:"r"`
}
type listDirArgs struct {
	Volume string `msgpack:"v"`
	Path   string `msgpack:"p"`
	Count  int    `msgpack:"c"`
}

type boolResult struct {
	OK bool `msgpack:"ok"`
}
type volInfoResult struct {
	Vol storage.VolInfo `msgpack:"vi"`
}
type volsResult struct {
	Vols []storage.VolInfo `msgpack:"vs"`
}
type bytesResult struct {
	Data []byte `msgpack:"d"`
}
type readFileResult struct {
	N    int    `msgpack:"n"`
	Data []byte `msgpack:"d"`
}
type fileStatResult struct {
	Stat storage.FileStat `msgpack:"fs"`
}
type listDirResult struct {
	Entries []string `msgpack:"e"`
}

// --- client (storage.StorageAPI over rpc) ----------------------------------

// Storage is a drive reached over rpc. It is the client half; Register installs
// the server half on the owning node.
type Storage struct {
	endpoint string
	client   *rpc.Client
}

var _ storage.StorageAPI = (*Storage)(nil)

// NewStorage returns a remote drive at endpoint (the peer's scheme://host[:port]),
// using hc for transport (nil for a default pooled client).
func NewStorage(endpoint string, hc *http.Client) *Storage {
	c := rpc.NewClient(endpoint, hc)
	c.DecodeErr = decodeErr
	return &Storage{endpoint: endpoint, client: c}
}

// String returns the peer endpoint.
func (s *Storage) String() string { return s.endpoint }

// IsOnline probes the peer with a short timeout, reporting false on any failure.
func (s *Storage) IsOnline() bool {
	ctx, cancel := context.WithTimeout(context.Background(), onlineTimeout)
	defer cancel()
	var res boolResult
	if err := s.unary(ctx, mIsOnline, struct{}{}, &res); err != nil {
		return false
	}
	return res.OK
}

// MakeVol implements storage.StorageAPI over RPC.
func (s *Storage) MakeVol(ctx context.Context, volume string) error {
	return s.void(ctx, mMakeVol, volArgs{Volume: volume})
}

// StatVol implements storage.StorageAPI over RPC.
func (s *Storage) StatVol(ctx context.Context, volume string) (storage.VolInfo, error) {
	var res volInfoResult
	err := s.unary(ctx, mStatVol, volArgs{Volume: volume}, &res)
	return res.Vol, err
}

// ListVols implements storage.StorageAPI over RPC.
func (s *Storage) ListVols(ctx context.Context) ([]storage.VolInfo, error) {
	var res volsResult
	err := s.unary(ctx, mListVols, struct{}{}, &res)
	return res.Vols, err
}

// DeleteVol implements storage.StorageAPI over RPC.
func (s *Storage) DeleteVol(ctx context.Context, volume string, force bool) error {
	return s.void(ctx, mDeleteVol, deleteVolArgs{Volume: volume, Force: force})
}

// ReadMeta implements storage.StorageAPI over RPC.
func (s *Storage) ReadMeta(ctx context.Context, volume, path string) ([]byte, error) {
	var res bytesResult
	err := s.unary(ctx, mReadMeta, pathArgs{Volume: volume, Path: path}, &res)
	return res.Data, err
}

// WriteMeta implements storage.StorageAPI over RPC. The payload travels in the
// request body frame, so a large obj.meta is not bound by any header limit.
func (s *Storage) WriteMeta(ctx context.Context, volume, path string, data []byte) error {
	return s.void(ctx, mWriteMeta, writeMetaArgs{Volume: volume, Path: path, Data: data})
}

// CreateFile implements storage.StorageAPI over RPC, streaming r to the peer as
// the request upload payload.
func (s *Storage) CreateFile(ctx context.Context, volume, path string, size int64, r io.Reader) error {
	reply, err := s.client.Call(ctx, mCreateFile, createFileArgs{Volume: volume, Path: path, Size: size}, r)
	if err != nil {
		return err
	}
	return reply.Close()
}

// ReadFile implements storage.StorageAPI over RPC, filling buf from the peer at
// the given offset (the result rides in the response frame, not a stream).
func (s *Storage) ReadFile(ctx context.Context, volume, path string, offset int64, buf []byte) (int, error) {
	var res readFileResult
	err := s.unary(ctx, mReadFile, readFileArgs{Volume: volume, Path: path, Offset: offset, Length: int64(len(buf))}, &res)
	if err != nil {
		return 0, err
	}
	return copy(buf, res.Data), nil
}

// ReadFileStream implements storage.StorageAPI over RPC, returning the peer's
// streamed payload. The caller closes the returned reader.
func (s *Storage) ReadFileStream(ctx context.Context, volume, path string, offset, length int64) (io.ReadCloser, error) {
	reply, err := s.client.Call(ctx, mReadFileStream, readFileArgs{Volume: volume, Path: path, Offset: offset, Length: length}, nil)
	if err != nil {
		return nil, err
	}
	return reply.Body, nil // the caller closes the stream
}

// RenameData implements storage.StorageAPI over RPC.
func (s *Storage) RenameData(ctx context.Context, volume, srcPath, dstPath string) error {
	return s.void(ctx, mRenameData, renameDataArgs{Volume: volume, SrcPath: srcPath, DstPath: dstPath})
}

// RenameFile implements storage.StorageAPI over RPC.
func (s *Storage) RenameFile(ctx context.Context, srcVolume, srcPath, dstVolume, dstPath string) error {
	return s.void(ctx, mRenameFile, renameFileArgs{SrcVolume: srcVolume, SrcPath: srcPath, DstVolume: dstVolume, DstPath: dstPath})
}

// Delete implements storage.StorageAPI over RPC.
func (s *Storage) Delete(ctx context.Context, volume, path string, recursive bool) error {
	return s.void(ctx, mDelete, deleteArgs{Volume: volume, Path: path, Recursive: recursive})
}

// StatFile implements storage.StorageAPI over RPC.
func (s *Storage) StatFile(ctx context.Context, volume, path string) (storage.FileStat, error) {
	var res fileStatResult
	err := s.unary(ctx, mStatFile, pathArgs{Volume: volume, Path: path}, &res)
	return res.Stat, err
}

// ListDir implements storage.StorageAPI over RPC.
func (s *Storage) ListDir(ctx context.Context, volume, path string, count int) ([]string, error) {
	var res listDirResult
	err := s.unary(ctx, mListDir, listDirArgs{Volume: volume, Path: path, Count: count}, &res)
	return res.Entries, err
}

// void issues a call with no result body.
func (s *Storage) void(ctx context.Context, method string, args any) error {
	reply, err := s.client.Call(ctx, method, args, nil)
	if err != nil {
		return err
	}
	return reply.Close()
}

// unary issues a call and decodes its result.
func (s *Storage) unary(ctx context.Context, method string, args, result any) error {
	reply, err := s.client.Call(ctx, method, args, nil)
	if err != nil {
		return err
	}
	defer func() { _ = reply.Close() }()
	return reply.Unmarshal(result)
}

// --- server (rpc handlers backed by a local drive) -------------------------

// Register installs the StorageAPI procedures on mux, each backed by local. Call
// it once per node with that node's local drive (or one Mux per drive, mounted on
// distinct endpoints) so peers can reach it.
func Register(mux *rpc.Mux, local storage.StorageAPI) {
	mux.EncodeErr = encodeErr

	mux.Register(mIsOnline, func(_ context.Context, _ msgpack.RawMessage, _ io.Reader, resp *rpc.Response) error {
		resp.SetResult(boolResult{OK: local.IsOnline()})
		return nil
	})
	mux.Register(mMakeVol, func(ctx context.Context, args msgpack.RawMessage, _ io.Reader, _ *rpc.Response) error {
		var a volArgs
		if err := msgpack.Unmarshal(args, &a); err != nil {
			return err
		}
		return local.MakeVol(ctx, a.Volume)
	})
	mux.Register(mStatVol, func(ctx context.Context, args msgpack.RawMessage, _ io.Reader, resp *rpc.Response) error {
		var a volArgs
		if err := msgpack.Unmarshal(args, &a); err != nil {
			return err
		}
		vi, err := local.StatVol(ctx, a.Volume)
		if err != nil {
			return err
		}
		resp.SetResult(volInfoResult{Vol: vi})
		return nil
	})
	mux.Register(mListVols, func(ctx context.Context, _ msgpack.RawMessage, _ io.Reader, resp *rpc.Response) error {
		vols, err := local.ListVols(ctx)
		if err != nil {
			return err
		}
		resp.SetResult(volsResult{Vols: vols})
		return nil
	})
	mux.Register(mDeleteVol, func(ctx context.Context, args msgpack.RawMessage, _ io.Reader, _ *rpc.Response) error {
		var a deleteVolArgs
		if err := msgpack.Unmarshal(args, &a); err != nil {
			return err
		}
		return local.DeleteVol(ctx, a.Volume, a.Force)
	})
	mux.Register(mReadMeta, func(ctx context.Context, args msgpack.RawMessage, _ io.Reader, resp *rpc.Response) error {
		var a pathArgs
		if err := msgpack.Unmarshal(args, &a); err != nil {
			return err
		}
		data, err := local.ReadMeta(ctx, a.Volume, a.Path)
		if err != nil {
			return err
		}
		resp.SetResult(bytesResult{Data: data})
		return nil
	})
	mux.Register(mWriteMeta, func(ctx context.Context, args msgpack.RawMessage, _ io.Reader, _ *rpc.Response) error {
		var a writeMetaArgs
		if err := msgpack.Unmarshal(args, &a); err != nil {
			return err
		}
		return local.WriteMeta(ctx, a.Volume, a.Path, a.Data)
	})
	mux.Register(mCreateFile, func(ctx context.Context, args msgpack.RawMessage, body io.Reader, _ *rpc.Response) error {
		var a createFileArgs
		if err := msgpack.Unmarshal(args, &a); err != nil {
			return err
		}
		return local.CreateFile(ctx, a.Volume, a.Path, a.Size, body)
	})
	mux.Register(mReadFile, func(ctx context.Context, args msgpack.RawMessage, _ io.Reader, resp *rpc.Response) error {
		var a readFileArgs
		if err := msgpack.Unmarshal(args, &a); err != nil {
			return err
		}
		buf := make([]byte, a.Length)
		n, err := local.ReadFile(ctx, a.Volume, a.Path, a.Offset, buf)
		if err != nil {
			return err
		}
		resp.SetResult(readFileResult{N: n, Data: buf[:n]})
		return nil
	})
	mux.Register(mReadFileStream, func(ctx context.Context, args msgpack.RawMessage, _ io.Reader, resp *rpc.Response) error {
		var a readFileArgs
		if err := msgpack.Unmarshal(args, &a); err != nil {
			return err
		}
		rc, err := local.ReadFileStream(ctx, a.Volume, a.Path, a.Offset, a.Length)
		if err != nil {
			return err
		}
		resp.SetStream(rc, rc)
		return nil
	})
	mux.Register(mRenameData, func(ctx context.Context, args msgpack.RawMessage, _ io.Reader, _ *rpc.Response) error {
		var a renameDataArgs
		if err := msgpack.Unmarshal(args, &a); err != nil {
			return err
		}
		return local.RenameData(ctx, a.Volume, a.SrcPath, a.DstPath)
	})
	mux.Register(mRenameFile, func(ctx context.Context, args msgpack.RawMessage, _ io.Reader, _ *rpc.Response) error {
		var a renameFileArgs
		if err := msgpack.Unmarshal(args, &a); err != nil {
			return err
		}
		return local.RenameFile(ctx, a.SrcVolume, a.SrcPath, a.DstVolume, a.DstPath)
	})
	mux.Register(mDelete, func(ctx context.Context, args msgpack.RawMessage, _ io.Reader, _ *rpc.Response) error {
		var a deleteArgs
		if err := msgpack.Unmarshal(args, &a); err != nil {
			return err
		}
		return local.Delete(ctx, a.Volume, a.Path, a.Recursive)
	})
	mux.Register(mStatFile, func(ctx context.Context, args msgpack.RawMessage, _ io.Reader, resp *rpc.Response) error {
		var a pathArgs
		if err := msgpack.Unmarshal(args, &a); err != nil {
			return err
		}
		fs, err := local.StatFile(ctx, a.Volume, a.Path)
		if err != nil {
			return err
		}
		resp.SetResult(fileStatResult{Stat: fs})
		return nil
	})
	mux.Register(mListDir, func(ctx context.Context, args msgpack.RawMessage, _ io.Reader, resp *rpc.Response) error {
		var a listDirArgs
		if err := msgpack.Unmarshal(args, &a); err != nil {
			return err
		}
		entries, err := local.ListDir(ctx, a.Volume, a.Path, a.Count)
		if err != nil {
			return err
		}
		resp.SetResult(listDirResult{Entries: entries})
		return nil
	})
}
