package nativehttp

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestKernelPeerUID(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "native-peer-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "peer.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	peer, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	uid, err := peerUID(peer)
	if err != nil || uid != os.Geteuid() {
		t.Fatalf("kernel UID = %d, %v; want %d", uid, err, os.Geteuid())
	}
	if err := checkPeer(peer); err != nil {
		t.Fatal(err)
	}
	peer.Close()
	if err := checkPeer(peer); err == nil {
		t.Fatal("accepted unavailable peer credentials")
	}
}
