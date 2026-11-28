package workerinstall

import (
	"encoding/binary"
	"testing"
)

func TestDarwinACLBufferBoundsAndMutationRights(t *testing.T) {
	empty := make([]byte, 12)
	binary.LittleEndian.PutUint32(empty, 12)
	if err := safeDarwinACL(empty); err != nil {
		t.Fatal(err)
	}
	entry := func(kind, rights uint32) []byte {
		b := make([]byte, 12+44+24)
		binary.LittleEndian.PutUint32(b, uint32(len(b)))
		binary.LittleEndian.PutUint32(b[4:], 8)
		binary.LittleEndian.PutUint32(b[8:], 68)
		binary.LittleEndian.PutUint32(b[12:], 0x012cc16d)
		binary.LittleEndian.PutUint32(b[48:], 1)
		binary.LittleEndian.PutUint32(b[72:], kind)
		binary.LittleEndian.PutUint32(b[76:], rights)
		return b
	}
	for _, b := range [][]byte{entry(2, 1<<4), entry(1, 1<<1|1<<3|1<<11)} {
		if err := safeDarwinACL(b); err != nil {
			t.Fatalf("non-mutating ACL: %v", err)
		}
	}
	for _, right := range []uint32{1 << 2, 1 << 4, 1 << 5, 1 << 6, 1 << 8, 1 << 10, 1 << 12, 1 << 13, 1 << 21, 1 << 23, 1 << 31} {
		for _, flags := range []uint32{1, 1 | 1<<8 | 1<<5} {
			if err := safeDarwinACL(entry(flags, right)); err == nil {
				t.Fatalf("accepted mutation right %#x flags %#x", right, flags)
			}
		}
	}
	for _, b := range [][]byte{nil, empty[:8], entry(3, 0), entry(1|1<<31, 0), entry(0, 0)} {
		if err := safeDarwinACL(b); err == nil {
			t.Fatal("accepted unknown or incomplete ACL")
		}
	}
	b := entry(2, 0)
	binary.LittleEndian.PutUint32(b, 8193)
	if err := safeDarwinACL(b); err == nil {
		t.Fatal("accepted truncated native result")
	}
	b = entry(2, 0)
	binary.LittleEndian.PutUint32(b[52:], 1<<16)
	if err := safeDarwinACL(b); err == nil {
		t.Fatal("accepted deferred inheritance")
	}
}
