package supervisor

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// aclFor is an access control list as Linux keeps one, which lets the user
// uid read besides the owner.
func aclFor(uid int) []byte {
	b := binary.LittleEndian.AppendUint32(nil, 2)
	for _, e := range []struct {
		tag, perm uint16
		id        uint32
	}{{1, 6, 1<<32 - 1}, {2, 4, uint32(uid)}, {4, 0, 1<<32 - 1}, {0x10, 4, 1<<32 - 1}, {0x20, 0, 1<<32 - 1}} {
		b = binary.LittleEndian.AppendUint16(b, e.tag)
		b = binary.LittleEndian.AppendUint16(b, e.perm)
		b = binary.LittleEndian.AppendUint32(b, e.id)
	}
	return b
}

// A file that is written keeps its access control list, and is given none
// that it had not; one whose list changes meanwhile is not written.
func TestFilesKeepsAccessList(t *testing.T) {
	f := newFilesFixture(t)
	set := func(name, attr string, uid int) {
		t.Helper()
		err := unix.Setxattr(filepath.Join(f.dir, name), attr, aclFor(uid), 0)
		if errors.Is(err, unix.ENOTSUP) {
			t.Skip("no access control lists here")
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	list := func(name string) access {
		t.Helper()
		fh, err := os.Open(filepath.Join(f.dir, name))
		if err != nil {
			t.Fatal(err)
		}
		defer fh.Close()
		a, err := accessOf(fh)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	put(t, f.dir, "notes/plan.md", "old")
	set("notes/plan.md", xattrACL, 4711)
	had := list("notes/plan.md")
	if had.acl != string(aclFor(4711)) || had.perm != 0o640 {
		t.Fatalf("the file has %+v", had)
	}
	made(t, f.own, writeOf("notes/plan.md", "new", versionOf("old")))
	made(t, f.own, editOf("notes/plan.md", "new", "newer"))
	if now := list("notes/plan.md"); now != had {
		t.Fatalf("written, the file has %+v, had %+v", now, had)
	}

	put(t, f.dir, "notes/own.md", "old")
	had = list("notes/own.md")
	set("notes", "system.posix_acl_default", 4711)
	made(t, f.own, writeOf("notes/own.md", "new", versionOf("old")))
	if now := list("notes/own.md"); now != had || now.acl != "" {
		t.Fatalf("written, the file has %+v, had %+v", now, had)
	}

	done := f.at(t, swap{"written", 1, func(t *testing.T, f *filesFixture) { set("notes/plan.md", xattrACL, 4712) }})
	f.misfit(t, f.own, writeOf("notes/plan.md", "other", versionOf("newer")), "the file changed while it was written", "newer")
	if !done() || list("notes/plan.md").acl != string(aclFor(4712)) {
		t.Fatal("the step was not reached, or the list was not kept")
	}
}
