package workerfiles

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const MaxFile = 8 << 20
const MaxHandles = 16

type Identity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

func Of(info os.FileInfo) (Identity, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return Identity{}, false
	}
	return Identity{Device: uint64(st.Dev), Inode: uint64(st.Ino)}, true
}
func New(root *os.Root, name string) (Identity, error) {
	file, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return Identity{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Identity{}, err
	}
	id, ok := Of(info)
	if !ok || id.Inode == 0 {
		return Identity{}, errors.New("file identity unavailable")
	}
	if err := file.Sync(); err != nil {
		return Identity{}, err
	}
	for _, path := range []string{filepath.Dir(name), filepath.Dir(filepath.Dir(name))} {
		directory, err := root.Open(path)
		if err != nil {
			return Identity{}, err
		}
		err = directory.Sync()
		closeErr := directory.Close()
		if err != nil {
			return Identity{}, err
		}
		if closeErr != nil {
			return Identity{}, closeErr
		}
	}
	return id, nil
}
func Open(root *os.Root, name string, id Identity, write bool) (*os.File, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	st, ok := before.Sys().(*syscall.Stat_t)
	if !ok || !before.Mode().IsRegular() || before.Mode().Perm()&0077 != 0 || st.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("file is not private and regular")
	}
	mode := os.O_RDONLY
	if write {
		mode = os.O_RDWR
	}
	f, err := root.OpenFile(name, mode|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	opened, err := f.Stat()
	got, valid := Identity{}, false
	if err == nil {
		got, valid = Of(opened)
	}
	if err != nil || !valid || got != id || id.Inode == 0 || !os.SameFile(before, opened) || opened.Size() < 0 || opened.Size() > MaxFile {
		f.Close()
		return nil, errors.New("file identity or size changed")
	}
	return f, nil
}
func Inspect(file *os.File) (uint64, string, error) {
	before, err := file.Stat()
	if err != nil || before.Size() < 0 || before.Size() > MaxFile {
		return 0, "", errors.New("file exceeds limit")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return 0, "", err
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(file, MaxFile+1))
	if err != nil || n > MaxFile || n != before.Size() {
		return 0, "", errors.New("file changed or exceeds limit")
	}
	after, err := file.Stat()
	if err != nil || after.Size() != n || !after.ModTime().Equal(before.ModTime()) {
		return 0, "", errors.New("file changed while hashing")
	}
	return uint64(n), hex.EncodeToString(hash.Sum(nil)), nil
}

// Source refuses traversal, every symlink component and nonregular files.
// Its root is an already registered workspace, never a main-side path.
func Source(root *os.Root, name string) (*os.File, error) {
	if name == "" || name == "." || filepath.IsAbs(name) || filepath.Clean(name) != name || strings.Contains(name, "\\") {
		return nil, errors.New("unsafe outgoing filename")
	}
	parts := strings.Split(filepath.ToSlash(name), "/")
	for i, part := range parts {
		if part == ".." || part == "." || part == "" {
			return nil, errors.New("unsafe outgoing filename")
		}
		info, err := root.Lstat(filepath.Join(parts[:i+1]...))
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || i < len(parts)-1 && !info.IsDir() || i == len(parts)-1 && !info.Mode().IsRegular() {
			return nil, errors.New("outgoing path is not a regular confined file")
		}
	}
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	f, err := root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !os.SameFile(before, info) || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > MaxFile {
		f.Close()
		return nil, errors.New("outgoing file identity/size changed")
	}
	return f, nil
}
