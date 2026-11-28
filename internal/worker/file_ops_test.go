package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/yousan/hataori/internal/workerfiles"
	"github.com/yousan/hataori/internal/workerproto"
)

func fileDigest(data []byte) string { hash := sha256.Sum256(data); return hex.EncodeToString(hash[:]) }

func TestWorkerUploadCapacityRefusesBeforeEffect(t *testing.T) {
	f := newWorkspaceFixture(t)
	data := bytes.Repeat([]byte("a"), workerfiles.MaxFile)
	digest := fileDigest(data)
	limited := false
	for file := 0; file < 2; file++ {
		handle := fmt.Sprintf("maximum-%d", file)
		for offset := 0; offset < len(data); offset += workerproto.MaxChunk {
			end := offset + workerproto.MaxChunk
			key := fmt.Sprintf("chunk-%d-%d", file, offset)
			m := f.request("upload", key, &f.target, workerproto.Upload{Handle: handle, Digest: digest, Size: uint64(len(data)), Offset: uint64(offset), Data: data[offset:end], Final: end == len(data)})
			reply := f.call(m)
			if reply.Kind == "refusal" {
				refused(t, reply, "limit")
				if file != 1 || f.r.state.Receipts[receiptKey(f.target, "upload", key)] != nil || !f.r.ready {
					t.Fatal("capacity refusal affected unrelated operations")
				}
				info, err := os.Stat(filepath.Join(f.root, f.r.filePath(f.target, handle)))
				if offset == 0 {
					if !errors.Is(err, os.ErrNotExist) {
						t.Fatal("refused upload created a file")
					}
				} else if err != nil || info.Size() != int64(offset) {
					t.Fatal("refused chunk changed bytes", err)
				}
				if record := f.r.state.Files[workspaceKey(f.target, handle)]; record != nil && (record.Offset != uint64(offset) || record.Complete) {
					t.Fatal("refused chunk advanced file")
				}
				limited = true
				break
			}
			var rec workerproto.Receipt
			if json.Unmarshal(reply.Body, &rec) != nil || rec.State != "complete" {
				t.Fatal("accepted chunk failed", string(reply.Body))
			}
		}
	}
	if !limited {
		t.Fatal("retained receipt capacity not enforced")
	}
	other := workerproto.Target{Thread: 89, Session: "capacity-session", Run: "capacity-run"}
	if rec := f.receipt(f.request("prepare", "capacity-prepare", &other, workerproto.Prepare{Policy: []string{}})); rec.State != "complete" {
		t.Fatal(rec)
	}
	f.restart()
	if !f.r.ready {
		t.Fatal("capacity refusal persisted a global failure")
	}
}

func TestWorkerUploadCountsUnjournaledFiles(t *testing.T) {
	f := newWorkspaceFixture(t)
	path := filepath.Join(f.root, f.r.runDir(f.target), "files")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < workerfiles.MaxHandles; i++ {
		if err := os.WriteFile(filepath.Join(path, fmt.Sprintf("orphan-%d", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	data := []byte("bounded")
	m := f.request("upload", "after-orphans", &f.target, workerproto.Upload{Handle: "new-file", Digest: fileDigest(data), Size: uint64(len(data)), Data: data, Final: true})
	refused(t, f.call(m), "limit")
	if f.r.state.Receipts[receiptKey(f.target, "upload", m.Key)] != nil {
		t.Fatal("orphan cap created intent")
	}
}
func TestWorkerBoundedUploadNeverOverwritesOnRetry(t *testing.T) {
	f := newWorkspaceFixture(t)
	data := []byte("accepted attachment")
	m := f.request("upload", "upload-one", &f.target, workerproto.Upload{Handle: "attachment-one", Digest: fileDigest(data), Size: uint64(len(data)), Data: data, Final: true})
	result := f.receipt(m)
	if result.State != "complete" {
		t.Fatal(result)
	}
	if again := f.receipt(m); again.RequestHash != result.RequestHash {
		t.Fatal("reply loss did not retain upload receipt")
	}
	if got, err := os.ReadFile(filepath.Join(f.root, f.r.filePath(f.target, "attachment-one"))); err != nil || string(got) != string(data) {
		t.Fatal("upload changed", err)
	}
	b := workerproto.Upload{Handle: "attachment-one", Digest: fileDigest(data), Size: uint64(len(data)), Data: data, Final: true}
	if reply := f.call(f.request("upload", "different-upload-key", &f.target, b)); reply.Kind != "refusal" {
		t.Fatal("completed upload was overwritten")
	}
	for _, bad := range []workerproto.Upload{
		{Handle: "../escape", Digest: fileDigest(data), Size: uint64(len(data)), Data: data, Final: true},
		{Handle: "absolute", Digest: fileDigest(data), Size: workerfiles.MaxFile + 1, Data: data},
	} {
		if f.call(f.request("upload", "invalid-"+bad.Handle, &f.target, bad)).Kind != "refusal" {
			t.Fatal("unbounded or traversing upload accepted")
		}
	}
	mismatch := f.receipt(f.request("upload", "mismatched-content", &f.target, workerproto.Upload{Handle: "mismatch", Digest: fileDigest([]byte("different")), Size: uint64(len(data)), Data: data, Final: true}))
	if mismatch.State != "refused" {
		t.Fatal("content mismatch accepted", mismatch)
	}
	t.Log("ran: bounded accepted-file upload, idempotent receipt, no overwrite, traversal/limit/digest refusal")
}
func TestWorkerUploadCrashDoesNotReplayOrAdoptFile(t *testing.T) {
	for _, point := range []string{"upload_before_write", "upload_after_write_before_sync", "upload_after_write"} {
		t.Run(point, func(t *testing.T) {
			f := newWorkspaceFixture(t)
			data := []byte("captured accepted bytes")
			m := f.request("upload", "upload-crash", &f.target, workerproto.Upload{Handle: "incoming-fixture", Digest: fileDigest(data), Size: uint64(len(data)), Data: data, Final: true})
			f.r.options.checkpoint = func(at string) error {
				if at == point {
					return errors.New("fixture crash")
				}
				return nil
			}
			if reply := f.call(m); reply.Kind != "refusal" {
				t.Fatal("crash reported success")
			}
			f.restart()
			rec := f.receipt(m)
			if point != "upload_before_write" && rec.State != "complete" {
				t.Fatal("durable identical chunk not reconciled", rec)
			}
			if point == "upload_before_write" && rec.State != "unknown" {
				t.Fatal("unperformed upload replayed", rec)
			}
		})
	}
}
func TestWorkerOutgoingFilesUseSessionHandles(t *testing.T) {
	f := newWorkspaceFixture(t)
	handle := f.clone()
	dir, _ := f.r.ownedWorkspace(f.target, handle)
	// A real worker file is captured; the caller gets no worker pathname.
	data := []byte("outgoing worker bytes")
	if err := os.WriteFile(filepath.Join(dir, "outgoing.txt"), data, 0600); err != nil {
		t.Fatal(err)
	}
	mainRoot := f.options.Config.Workspaces.Roots[1].Path
	if err := os.MkdirAll(mainRoot, 0700); err != nil {
		t.Fatal(err)
	}
	mainPath := filepath.Join(mainRoot, "outgoing.txt")
	if err := os.WriteFile(mainPath, []byte("main-only bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	outgoing, err := f.r.CaptureOutgoing(context.Background(), f.target, handle, "outgoing.txt")
	if err != nil {
		t.Fatal(err)
	}
	reply := f.call(f.request("download", "", &f.target, workerproto.Download{Handle: outgoing, Limit: workerproto.MaxChunk}))
	var got workerproto.FileBytes
	if reply.Kind != "reply" || json.Unmarshal(reply.Body, &got) != nil || string(got.Data) != string(data) || got.Digest != fileDigest(data) {
		t.Fatalf("outgoing: %s", reply.Body)
	}
	// The file on the worker may change after capture; the bounded snapshot is
	// immutable and independent of any similarly spelled file on the main.
	if err := os.WriteFile(filepath.Join(dir, "outgoing.txt"), []byte("changed worker source"), 0600); err != nil {
		t.Fatal(err)
	}
	again := f.call(f.request("download", "", &f.target, workerproto.Download{Handle: outgoing, Limit: workerproto.MaxChunk}))
	if json.Unmarshal(again.Body, &got) != nil || !bytes.Equal(got.Data, data) {
		t.Fatal("snapshot read a main pathname or changed worker file")
	}
	if local, err := os.ReadFile(mainPath); err != nil || string(local) != "main-only bytes" {
		t.Fatal("transfer changed local main file", err)
	}
	if err := os.Symlink("outgoing.txt", filepath.Join(dir, "linked")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../escape", "/absolute", "linked", "."} {
		if _, err := f.r.CaptureOutgoing(context.Background(), f.target, handle, name); err == nil {
			t.Fatal("unsafe outgoing path accepted", name)
		}
	}
	other := workerproto.Target{Thread: 88, Session: "other-session", Run: "other-run"}
	if rec := f.receipt(f.request("prepare", "other-session", &other, workerproto.Prepare{Policy: []string{}})); rec.State != "complete" {
		t.Fatal(rec)
	}
	if f.call(f.request("download", "", &other, workerproto.Download{Handle: outgoing, Limit: 32})).Kind != "refusal" {
		t.Fatal("cross-session outgoing handle accepted")
	}
	// A replacement with different bytes/inode is not served from a remembered
	// path. Keep the original inode allocated to avoid accidental inode reuse.
	path := filepath.Join(f.root, f.r.filePath(f.target, outgoing))
	if err := os.Rename(path, path+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("foreign file"), 0600); err != nil {
		t.Fatal(err)
	}
	if f.call(f.request("download", "", &f.target, workerproto.Download{Handle: outgoing, Limit: 32})).Kind != "refusal" {
		t.Fatal("replacement file served")
	}
	t.Log("ran: outgoing snapshot and digest via session handle; traversal/symlink/cross-session/replacement rejected")
}

func TestWorkerPartialUploadCannotOverwriteAcceptedBytes(t *testing.T) {
	f := newWorkspaceFixture(t)
	data := []byte("first-second")
	digest := fileDigest(data)
	first := workerproto.Upload{Handle: "partial", Digest: digest, Size: uint64(len(data)), Data: data[:6], Final: false}
	if rec := f.receipt(f.request("upload", "first-partial", &f.target, first)); rec.State != "complete" {
		t.Fatal(rec)
	}
	first.Data = []byte("other-")
	refused(t, f.call(f.request("upload", "replayed-partial", &f.target, first)), "stale")
	path := filepath.Join(f.root, f.r.filePath(f.target, first.Handle))
	if got, err := os.ReadFile(path); err != nil || string(got) != "first-" {
		t.Fatal("accepted bytes overwritten", err)
	}
	second := workerproto.Upload{Handle: first.Handle, Digest: digest, Size: uint64(len(data)), Offset: 6, Data: data[6:], Final: true}
	if rec := f.receipt(f.request("upload", "second-partial", &f.target, second)); rec.State != "complete" {
		t.Fatal(rec)
	}
}

func TestWorkerUploadRecoveryRequiresSuccessfulSync(t *testing.T) {
	f := newWorkspaceFixture(t)
	data := []byte("cached but not yet acknowledged")
	message := f.request("upload", "sync-recovery", &f.target, workerproto.Upload{Handle: "sync-fixture", Digest: fileDigest(data), Size: uint64(len(data)), Data: data, Final: true})
	f.r.options.checkpoint = func(at string) error {
		if at == "upload_after_write_before_sync" {
			return errors.New("fixture crash")
		}
		return nil
	}
	if f.call(message).Kind != "refusal" {
		t.Fatal("crash acknowledged unsynced upload")
	}
	called := false
	f.options.syncUploadedFile = func(*os.File) error { called = true; return errors.New("fixture fsync failure") }
	f.restart()
	if rec := f.receipt(message); !called || rec.State != "unknown" || f.r.ready {
		t.Fatal("unsynced bytes acknowledged", rec, called)
	}
}

func TestWorkerConcurrentSessionFileAdmission(t *testing.T) {
	f := newWorkspaceFixture(t)
	workspace := f.clone()
	dir, _ := f.r.ownedWorkspace(f.target, workspace)
	if err := os.WriteFile(filepath.Join(dir, "out.txt"), []byte("outgoing"), 0600); err != nil {
		t.Fatal(err)
	}
	errors := make(chan error, 16)
	for i := 0; i < 8; i++ {
		data := []byte("incoming")
		message := f.request("upload", fmt.Sprintf("concurrent-%d", i), &f.target, workerproto.Upload{Handle: fmt.Sprintf("incoming-%d", i), Digest: fileDigest(data), Size: uint64(len(data)), Data: data, Final: true})
		frame := testFrame(t, message)
		go func() {
			reply := f.r.Dispatch(context.Background(), f.connection, frame)
			var rec workerproto.Receipt
			if reply.Kind != "reply" || json.Unmarshal(reply.Body, &rec) != nil || rec.State != "complete" {
				errors <- fmt.Errorf("concurrent upload: %s", reply.Body)
			} else {
				errors <- nil
			}
		}()
		go func() {
			_, err := f.r.CaptureOutgoing(context.Background(), f.target, workspace, "out.txt")
			errors <- err
		}()
	}
	for i := 0; i < 16; i++ {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	if len(f.r.state.Files) != 16 {
		t.Fatal("concurrent admission lost records")
	}
}
