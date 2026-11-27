package worker

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/yousan/hataori/internal/workerfiles"
	"github.com/yousan/hataori/internal/workerproto"
)

type fileRecord struct {
	Target   workerproto.Target   `json:"target"`
	Handle   string               `json:"handle"`
	Identity workerfiles.Identity `json:"identity"`
	Digest   string               `json:"digest"`
	Size     uint64               `json:"size"`
	Offset   uint64               `json:"offset"`
	Complete bool                 `json:"complete"`
	Outgoing bool                 `json:"outgoing"`
}

func fileIdentity(info os.FileInfo) (uint64, uint64, bool) {
	id, ok := workerfiles.Of(info)
	return id.Device, id.Inode, ok
}
func (r *Resident) filePath(t workerproto.Target, handle string) string {
	return filepath.Join(r.runDir(t), "files", handle)
}
func (r *Resident) validateFiles() error {
	for key, f := range r.state.Files {
		if f == nil || workerproto.CheckTarget(f.Target, f.Target) != nil || !validHandle(f.Handle) || key != workspaceKey(f.Target, f.Handle) || f.Size > workerfiles.MaxFile || f.Offset > f.Size || f.Identity.Inode == 0 || len(f.Digest) != 64 {
			return errors.New("worker: invalid session file journal")
		}
		digest, err := hex.DecodeString(f.Digest)
		if err != nil || len(digest) != 32 || f.Complete && f.Offset != f.Size {
			return errors.New("worker: invalid session file digest/offset")
		}
	}
	return nil
}

// Count both journaled handles and unclaimed directory entries: a crash before
// identity journaling must not permit unbounded orphan creation on retry.
func (r *Resident) fileSlots(t workerproto.Target) (int, error) {
	names := map[string]bool{}
	for _, f := range r.state.Files {
		if f.Target == t {
			names[f.Handle] = true
		}
	}
	dir, err := r.root.Open(filepath.Join(r.runDir(t), "files"))
	if errors.Is(err, os.ErrNotExist) {
		return len(names), nil
	}
	if err != nil {
		return 0, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(workerfiles.MaxHandles + 1)
	if err != nil && err != io.EOF {
		return 0, err
	}
	for _, entry := range entries {
		names[entry.Name()] = true
	}
	return len(names), nil
}

func (r *Resident) admitUpload(m workerproto.Message) bool {
	// Reserve four MiB for reconciliation, bounded result metadata and other
	// operations. Receipts intentionally retain accepted chunks until run GC.
	current, err := json.Marshal(r.state)
	receipt, e := json.Marshal(receiptRecord{Request: m, Receipt: workerproto.Receipt{Operation: m.Op, Key: m.Key, RequestHash: string(bytes.Repeat([]byte("0"), 64)), State: "pending"}})
	return err == nil && e == nil && len(current)+len(receipt)+4096 < maxJournal-(4<<20)
}
func (r *Resident) upload(m workerproto.Message) *workerproto.Refusal {
	t := *m.Target
	var b workerproto.Upload
	json.Unmarshal(m.Body, &b)
	if b.Size > workerfiles.MaxFile {
		return failure("limit", "file exceeds worker transfer limit")
	}
	run := r.state.Runs[targetKey(t)]
	if run == nil || r.checkMarker(run) != nil {
		return failure("wrong_target", "session file owner is unavailable")
	}
	key := workspaceKey(t, b.Handle)
	record := r.state.Files[key]
	if record != nil && (record.Target != t || record.Outgoing || record.Complete || record.Digest != b.Digest || record.Size != b.Size || record.Offset != b.Offset) {
		return failure("stale", "upload does not match the registered file offset/content")
	}
	if record == nil && b.Offset != 0 {
		return failure("stale", "upload must start at zero")
	}
	if record == nil {
		n, err := r.fileSlots(t)
		if err != nil {
			return failure("unavailable", "session file directory cannot be inspected")
		}
		if n >= workerfiles.MaxHandles {
			return failure("limit", "session file handle limit reached")
		}
	}
	if !r.admitUpload(m) {
		return failure("limit", "retained upload receipt capacity reached")
	}
	rec, err := r.intent(m)
	if err != nil {
		return failure("unavailable", "upload intent could not be committed")
	}
	if record == nil {
		if err := privateDir(r.root, filepath.Join(r.runDir(t), "files")); err != nil {
			return r.fileFailure(rec, err, "session file directory is unavailable")
		}
		id, err := workerfiles.New(r.root, r.filePath(t, b.Handle))
		if err != nil {
			return r.fileFailure(rec, err, "upload destination already exists or is unavailable")
		}
		record = &fileRecord{Target: t, Handle: b.Handle, Identity: id, Digest: b.Digest, Size: b.Size}
		r.state.Files[key] = record
		if err := r.save(); err != nil {
			return r.fileFailure(rec, err, "upload identity could not be committed")
		}
		if err := r.point("upload_identity"); err != nil {
			return r.fileFailure(rec, err, "upload outcome is uncertain")
		}
	}
	f, err := workerfiles.Open(r.root, r.filePath(t, b.Handle), record.Identity, true)
	if err != nil {
		return r.fileFailure(rec, err, "upload identity changed")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || uint64(info.Size()) != b.Offset {
		return r.refuseFile(rec, "stale", "upload would overwrite existing bytes")
	}
	if err := r.point("upload_before_write"); err != nil {
		return r.fileFailure(rec, err, "upload outcome is uncertain")
	}
	if _, err := f.WriteAt(b.Data, int64(b.Offset)); err != nil {
		return r.fileFailure(rec, err, "upload outcome is uncertain")
	}
	if err := r.point("upload_after_write_before_sync"); err != nil {
		return r.fileFailure(rec, err, "upload durability is uncertain")
	}
	if err := r.syncUploaded(f); err != nil {
		return r.fileFailure(rec, err, "upload durability is uncertain")
	}
	if err := r.point("upload_after_write"); err != nil {
		return r.fileFailure(rec, err, "upload outcome is uncertain")
	}
	if b.Final {
		size, digest, err := workerfiles.Inspect(f)
		if err != nil || size != b.Size || digest != b.Digest {
			return r.refuseFile(rec, "denied", "upload content does not match its complete-file digest")
		}
	}
	record.Offset = b.Offset + uint64(len(b.Data))
	record.Complete = b.Final
	return r.complete(rec, workerproto.Effect{Handle: b.Handle, Digest: b.Digest, Size: b.Size})
}
func (r *Resident) recoverUpload(rec *receiptRecord) error {
	var b workerproto.Upload
	json.Unmarshal(rec.Request.Body, &b)
	t := *rec.Request.Target
	record := r.state.Files[workspaceKey(t, b.Handle)]
	if record == nil || record.Target != t || record.Outgoing || record.Digest != b.Digest || record.Size != b.Size {
		return errors.New("worker: upload identity is unknown")
	}
	f, err := workerfiles.Open(r.root, r.filePath(t, b.Handle), record.Identity, false)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	end := b.Offset + uint64(len(b.Data))
	if uint64(info.Size()) != end {
		return errors.New("worker: upload did not durably reach its recorded chunk end")
	}
	data := make([]byte, len(b.Data))
	if _, err := f.ReadAt(data, int64(b.Offset)); err != nil && !(len(data) == 0 && err == io.EOF) {
		return err
	}
	if !bytes.Equal(data, b.Data) {
		return errors.New("worker: upload bytes differ from the accepted chunk")
	}
	if b.Final {
		size, digest, err := workerfiles.Inspect(f)
		if err != nil || size != b.Size || digest != b.Digest {
			return errors.New("worker: final upload content differs")
		}
	}
	// Matching cached bytes after a process crash are not yet durable.
	if err := r.syncUploaded(f); err != nil {
		return err
	}
	if err := syncDir(r.root, filepath.Dir(r.filePath(t, b.Handle))); err != nil {
		return err
	}
	record.Offset = end
	record.Complete = b.Final
	rec.Receipt.State = "complete"
	rec.Receipt.Refusal = nil
	rec.Receipt.Result = &workerproto.Effect{Handle: b.Handle, Digest: b.Digest, Size: b.Size}
	return nil
}
func (r *Resident) download(t workerproto.Target, b workerproto.Download) (workerproto.FileBytes, *workerproto.Refusal) {
	record := r.state.Files[workspaceKey(t, b.Handle)]
	if record == nil || record.Target != t || !record.Complete || !record.Outgoing {
		return workerproto.FileBytes{}, failure("wrong_target", "outgoing handle is not registered for this run")
	}
	if b.Offset > record.Size || b.Limit == 0 || b.Limit > workerproto.MaxChunk {
		return workerproto.FileBytes{}, failure("limit", "download range exceeds its limit")
	}
	f, err := workerfiles.Open(r.root, r.filePath(t, b.Handle), record.Identity, false)
	if err != nil {
		return workerproto.FileBytes{}, failure("replaced", "outgoing file identity changed")
	}
	defer f.Close()
	size, digest, err := workerfiles.Inspect(f)
	if err != nil || size != record.Size || digest != record.Digest {
		return workerproto.FileBytes{}, failure("replaced", "outgoing content changed")
	}
	length := uint64(b.Limit)
	if size-b.Offset < length {
		length = size - b.Offset
	}
	data := make([]byte, int(length))
	if len(data) > 0 {
		if _, err := f.ReadAt(data, int64(b.Offset)); err != nil {
			return workerproto.FileBytes{}, failure("unavailable", "outgoing bytes could not be read")
		}
	}
	return workerproto.FileBytes{Handle: b.Handle, Digest: digest, Size: size, Offset: b.Offset, Data: data, Final: b.Offset+length == size}, nil
}

// CaptureOutgoing snapshots an adapter-selected file from the receiver-owned
// workspace. Callers receive only a session-bound handle and bounded bytes via
// download; a worker pathname is never returned for the main to open.
func (r *Resident) CaptureOutgoing(ctx context.Context, t workerproto.Target, workspaceHandle, relative string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || !r.ready || r.visibleRoot() != nil || ctx.Err() != nil {
		return "", errors.New("worker: outgoing capture unavailable")
	}
	_, err := r.ownedWorkspace(t, workspaceHandle)
	if err != nil {
		return "", err
	}
	n, err := r.fileSlots(t)
	if err != nil {
		return "", err
	}
	if n >= workerfiles.MaxHandles {
		return "", errors.New("worker: session file limit reached")
	}
	sourceRoot, err := r.root.OpenRoot(filepath.Join("workspaces", workspaceHandle))
	if err != nil {
		return "", err
	}
	defer sourceRoot.Close()
	registered := r.state.Workspaces[workspaceKey(t, workspaceHandle)]
	info, err := sourceRoot.Stat(".")
	if err != nil {
		return "", err
	}
	dev, ino, ok := fileIdentity(info)
	if !ok || dev != registered.Device || ino != registered.Inode {
		return "", errors.New("worker: outgoing workspace identity changed")
	}
	source, err := workerfiles.Source(sourceRoot, relative)
	if err != nil {
		return "", err
	}
	defer source.Close()
	if err := privateDir(r.root, filepath.Join(r.runDir(t), "files")); err != nil {
		return "", err
	}
	handle := newID()
	id, err := workerfiles.New(r.root, r.filePath(t, handle))
	if err != nil {
		return "", err
	}
	record := &fileRecord{Target: t, Handle: handle, Identity: id, Outgoing: true, Digest: string(bytes.Repeat([]byte("0"), 64))}
	r.state.Files[workspaceKey(t, handle)] = record
	if err := r.save(); err != nil {
		r.ready = false
		return "", err
	}
	destination, err := workerfiles.Open(r.root, r.filePath(t, handle), id, true)
	if err != nil {
		return "", err
	}
	defer destination.Close()
	before, err := source.Stat()
	if err != nil {
		return "", err
	}
	copied, err := io.Copy(destination, io.LimitReader(source, workerfiles.MaxFile+1))
	if err != nil || copied > workerfiles.MaxFile || ctx.Err() != nil {
		return "", errors.New("worker: outgoing file exceeds limit or capture failed")
	}
	after, err := source.Stat()
	if err != nil || before.Size() != copied || after.Size() != copied || !before.ModTime().Equal(after.ModTime()) {
		return "", errors.New("worker: outgoing source changed while capturing")
	}
	if err := destination.Sync(); err != nil {
		return "", err
	}
	size, digest, err := workerfiles.Inspect(destination)
	if err != nil {
		return "", err
	}
	record.Size = size
	record.Offset = size
	record.Digest = digest
	record.Complete = true
	if err := r.save(); err != nil {
		r.ready = false
		return "", err
	}
	return handle, nil
}
