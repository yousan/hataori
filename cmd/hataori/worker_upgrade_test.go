package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/agent/fake"
	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/workerinstall"
	"github.com/yousan/hataori/internal/workerproto"
)

// The resident is reopened with the explicitly selected future host command;
// the main really exits and restarts through the isolated first-worker fixture.
// No service manager, real agent, tmux or existing installation is involved.
func TestWorkerUpgradeRetainsRunningHostAndWaitingHook(t *testing.T) {
	release := newWorkerReleaseFixture(t)
	old := release.install(t, "v1")
	f := newFirstWorker(t, func(cfg *fake.Config) { cfg.Command = []string{old.Path, "fake"}; cfg.Hataori = old.Path })
	f.stopResident()
	f.options.HostCommand.Path = old.Path
	f.startResident()
	c := f.client()
	id, conv := f.start(c, "upgrade-waiting-hook", "ask upgrade question\nreply survived explicit upgrade", "build")
	f.main.postedWith(t, conv, "upgrade question")
	detail, err := c.Thread(context.Background(), id, 0, 0)
	if err != nil || len(detail.Requests) != 1 {
		t.Fatalf("original question: %+v %v", detail.Requests, err)
	}
	var beforeStarts, beforeEnds int
	if err := f.main.db(t).QueryRow(`SELECT count(*),count(ended_at) FROM turns x JOIN sessions s ON s.id=x.session_id WHERE s.thread_id=?`, id).Scan(&beforeStarts, &beforeEnds); err != nil || beforeStarts != 1 || beforeEnds != 1 {
		t.Fatalf("question must end the original turn as blocked: %d %d %v", beforeStarts, beforeEnds, err)
	}
	paths, err := filepath.Glob(filepath.Join(f.root, "state/sessions/*", host.SpecFile))
	if err != nil || len(paths) != 1 {
		t.Fatalf("owned host paths: %v %v", paths, err)
	}
	hostDir := filepath.Dir(paths[0])
	spec, ok, err := host.ReadSpec(hostDir)
	if err != nil || !ok || len(spec.Argv) == 0 || spec.Argv[0] != old.Path {
		t.Fatalf("original executable: %+v %v", spec, err)
	}
	owned, err := host.DialIdentity(context.Background(), hostDir, spec.Identity)
	if err != nil {
		t.Fatal(err)
	}
	defer owned.Close()
	owned.Deadline(time.Now().Add(20 * time.Second))
	waiting, err := owned.Waiting()
	if err != nil || len(waiting) != 1 {
		t.Fatalf("waiting hook: %+v %v", waiting, err)
	}
	oldInfo, err := os.Stat(old.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"wrong-platform", "checksum", "provenance", "partial-download", "failed-publication"} {
		h := sha256.Sum256(release.binary)
		m := workerinstall.Manifest{Format: 1, Version: kind, Protocol: workerproto.Version, Artifacts: []workerinstall.Artifact{{Platform: workerPlatform(), SHA256: hex.EncodeToString(h[:]), Size: int64(len(release.binary))}}}
		raw, _ := json.Marshal(m)
		req := workerinstall.Request{Manifest: raw, Signature: ed25519.Sign(release.key, raw), Trusted: release.key.Public().(ed25519.PublicKey), Platform: workerPlatform(), Protocol: workerproto.Version, Binary: bytes.NewReader(release.binary)}
		switch kind {
		case "wrong-platform":
			req.Platform.Arch = "unsupported"
		case "checksum":
			b := bytes.Clone(release.binary)
			b[4096] ^= 1
			req.Binary = bytes.NewReader(b)
		case "provenance":
			req.Signature = make([]byte, ed25519.SignatureSize)
		case "partial-download":
			req.Binary = bytes.NewReader(release.binary[:len(release.binary)/2])
		case "failed-publication":
			if err := os.Mkdir(filepath.Join(release.prefix, "versions", m.Version+"-"+m.Artifacts[0].SHA256), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := workerinstall.Install(release.prefix, req); err == nil {
			t.Fatalf("accepted %s", kind)
		}
		selected, err := workerinstall.Selected(release.prefix, workerPlatform(), workerproto.Version)
		if err != nil || selected.Path != old.Path {
			t.Fatalf("%s changed selection: %+v %v", kind, selected, err)
		}
		after, err := owned.Waiting()
		if err != nil || len(after) != 1 || after[0].ID != waiting[0].ID {
			t.Fatalf("%s changed live host/hook: %+v %v", kind, after, err)
		}
	}
	current := release.install(t, "v2")
	if current.Path == old.Path {
		t.Fatal("upgrade reused old executable path")
	}
	// Explicit resident shutdown/reopen changes future launches only. It must
	// not close the host's own socket or answer/cancel the outstanding hook.
	c.Close()
	f.stopResident()
	f.options.HostCommand.Path = current.Path
	f.startResident()
	f.restart()
	after, err := owned.Waiting()
	if err != nil || len(after) != 1 || after[0].ID != waiting[0].ID {
		t.Fatalf("upgrade/restart lost original hook: %+v %v", after, err)
	}
	info, err := os.Stat(old.Path)
	if err != nil || !os.SameFile(oldInfo, info) {
		t.Fatalf("old hook/host executable removed or replaced: %v", err)
	}
	selected, err := workerinstall.Selected(release.prefix, workerPlatform(), "1.3")
	if err != nil || selected.Path != current.Path {
		t.Fatalf("future launch selection: %+v %v", selected, err)
	}
	c = f.client()
	defer c.Close()
	f.ready(c, id)
	if err := f.main.db(t).QueryRow(`SELECT count(*),count(ended_at) FROM turns x JOIN sessions s ON s.id=x.session_id WHERE s.thread_id=?`, id).Scan(&beforeStarts, &beforeEnds); err != nil || beforeStarts != 1 || beforeEnds != 1 {
		t.Fatalf("upgrade/restart duplicated the blocked turn: %d %d %v", beforeStarts, beforeEnds, err)
	}
	if err := c.Answer(context.Background(), detail.Requests[0].ID, control.Answer{Text: "blue"}); err != nil {
		t.Fatal(err)
	}
	f.main.posted(t, conv, "survived explicit upgrade")
	// A question closes its turn as blocked; the answer creates one
	// continuation. Neither the upgrade nor replay may add another.
	f.turnCount(id, 2)
	if f.inputCount("ask upgrade question\nreply survived explicit upgrade") != 1 {
		t.Fatal("upgrade/restart replayed original input")
	}
	if _, err := os.Stat(old.Path); err != nil {
		t.Fatal("old version removed after hook completion")
	}
	t.Log("ran: explicit verified v1→v2 selection; detached versioned fake host and original hook socket survive resident close/reopen and actual main restart; original question answered once; all old binary paths retained")
}
