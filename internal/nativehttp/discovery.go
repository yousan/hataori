package nativehttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"syscall"
)

// discovery is private transport metadata, not a control-protocol message.
// A pathname alone never authorizes a connection: all binding fields, the
// expected pathname, filesystem identity and both peers' credentials matter.
type discovery struct {
	Format     int    `json:"format"`
	Root       string `json:"root"`
	RootDevice uint64 `json:"root_device"`
	RootInode  uint64 `json:"root_inode"`
	Instance   string `json:"instance"`
	Version    string `json:"version"`
	Generation string `json:"generation"`
	Socket     string `json:"socket"`
	Device     uint64 `json:"device"`
	Inode      uint64 `json:"inode"`
}

func readDiscovery(state, root *os.Root, instance string) (discovery, error) {
	f, err := privateFile(state, DiscoveryFile, os.O_RDONLY)
	if err != nil {
		return discovery{}, fmt.Errorf("native HTTP: discovery: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return discovery{}, err
	}
	if info.Size() > 8192 {
		return discovery{}, errors.New("native HTTP: discovery is too large")
	}
	dec := json.NewDecoder(io.LimitReader(f, 8193))
	dec.DisallowUnknownFields()
	var d discovery
	if err := dec.Decode(&d); err != nil {
		return d, fmt.Errorf("native HTTP: discovery: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return d, errors.New("native HTTP: trailing discovery data")
	}
	rootInfo, err := root.Stat(".")
	if err != nil {
		return d, err
	}
	rootStat, ok := rootInfo.Sys().(*syscall.Stat_t)
	if !ok || d.RootDevice != uint64(rootStat.Dev) || d.RootInode != uint64(rootStat.Ino) {
		return d, errors.New("native HTTP: root filesystem identity changed")
	}
	if d.Format != 1 || d.Root != root.Name() || d.Instance != instance || !validText(d.Version) || !validGeneration(d.Generation) || d.Inode == 0 {
		return d, errors.New("native HTTP: discovery binding mismatch")
	}
	return d, nil
}

func validGeneration(s string) bool {
	if len(s) != 24 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func publish(state, root *os.Root, d discovery) error {
	name := ".native-" + d.Generation + ".json"
	f, err := privateFile(state, name, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return err
	}
	defer f.Close()
	defer state.Remove(name)
	if err := json.NewEncoder(f).Encode(d); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if _, err := readDiscovery(state, root, d.Instance); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return state.Rename(name, DiscoveryFile)
}

// Dial validates private discovery against the exact selected root,
// installation and version, then checks the server's kernel UID. It never
// starts a daemon, repairs permissions, deletes a lock or retries a mutation.
func Dial(ctx context.Context, cfg Config) (net.Conn, error) {
	if err := validConfig(cfg); err != nil {
		return nil, err
	}
	r, err := openPrivateRoot(cfg.Root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	state, err := privateDir(r, "state", false)
	if err != nil {
		return nil, err
	}
	defer state.Close()
	d, err := readDiscovery(state, r, cfg.Instance)
	if err != nil {
		return nil, err
	}
	if d.Version != cfg.Version {
		return nil, fmt.Errorf("native HTTP: incompatible instance version %q; expected %q", d.Version, cfg.Version)
	}
	expected, err := socketPath(r, cfg, d.Generation, false)
	if err != nil {
		return nil, err
	}
	if d.Socket != expected {
		return nil, errors.New("native HTTP: redirected socket discovery")
	}
	check := func() error {
		info, err := os.Lstat(expected)
		if err != nil {
			return err
		}
		if err := private(info, fs.ModeSocket, 0o600); err != nil {
			return err
		}
		if err := checkACL(expected); err != nil {
			return err
		}
		stat := info.Sys().(*syscall.Stat_t)
		if uint64(stat.Dev) != d.Device || uint64(stat.Ino) != d.Inode {
			return errors.New("native HTTP: socket identity changed")
		}
		return nil
	}
	if err := check(); err != nil {
		return nil, err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", expected)
	if err != nil {
		return nil, err
	}
	if err := checkPeer(conn.(*net.UnixConn)); err != nil {
		conn.Close()
		return nil, err
	}
	if err := check(); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}
