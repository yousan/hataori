package workerinstall

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Request is available only to the operator command, never a worker RPC.
// The trust key is supplied independently of the downloaded manifest.
type Request struct {
	Manifest  []byte
	Signature []byte
	Trusted   ed25519.PublicKey
	Platform  Platform
	Protocol  string
	Binary    io.Reader
}

type Selection struct {
	Manifest  []byte   `json:"manifest"`
	Signature []byte   `json:"signature"`
	Platform  Platform `json:"platform"`
}

type Result struct {
	Path    string `json:"path"`
	Prefix  string `json:"prefix"`
	Version string `json:"version"`
	// Once the atomic selector rename succeeds the installation is selected.
	// A subsequent directory sync failure must not be reported as a refusal
	// with an unchanged selection; the operator receives the committed state.
	DurabilityWarning string `json:"durability_warning,omitempty"`
}

type store struct {
	root *os.Root
	lock *os.File
	path string
	key  ed25519.PublicKey
}

func owned(info os.FileInfo, dir bool) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0o077 != 0 || info.IsDir() != dir || !dir && !info.Mode().IsRegular() {
		return errors.New("worker install: expected private owned regular file or directory")
	}
	return nil
}

func openStore(path string, key ed25519.PublicKey, create bool) (_ *store, err error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("worker install: installation prefix must be an explicit canonical absolute path")
	}
	// Resolve aliases before creating/opening anything. The returned executable
	// and service prefix must never re-resolve an operator's mutable alias.
	physicalParent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	if err = secureAncestors(physicalParent); err != nil {
		return nil, err
	}
	path = filepath.Join(physicalParent, filepath.Base(path))
	if create {
		if e := os.Mkdir(path, 0o700); e != nil && !errors.Is(e, os.ErrExist) {
			return nil, e
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if err := owned(info, true); err != nil {
		return nil, err
	}
	physical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	// The final prefix itself cannot be a link (owned checked Lstat above).
	// Resolve OS-provided ancestors, such as macOS /var, and pin the directory.
	r, err := os.OpenRoot(physical)
	if err != nil {
		return nil, err
	}
	s := &store{root: r, path: path}
	defer func() {
		if err != nil {
			s.close()
		}
	}()
	opened, err := r.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("worker install: prefix changed while opening")
	}
	// Only the marker and files below this dedicated prefix are ours. Never
	// adopt a worker root, an arbitrary directory, or an unmarked partial tree.
	marker, err := s.read("trust-key", ed25519.PublicKeySize)
	if errors.Is(err, os.ErrNotExist) && create {
		d, e := r.Open(".")
		if e != nil {
			return nil, e
		}
		entries, e := d.ReadDir(1)
		d.Close()
		if len(entries) != 0 || e != io.EOF {
			return nil, errors.New("worker install: refusing nonempty unregistered prefix")
		}
		if len(key) != ed25519.PublicKeySize {
			return nil, errors.New("worker install: missing trust key")
		}
		if err = s.newFile("trust-key", key, 0o400); err != nil {
			return nil, err
		}
		marker = key
	} else if err != nil {
		return nil, err
	}
	if len(marker) != ed25519.PublicKeySize || key != nil && !bytes.Equal(marker, key) {
		return nil, errors.New("worker install: installation trust key mismatch")
	}
	s.key = ed25519.PublicKey(marker)
	if err = s.dir("versions", create); err != nil {
		return nil, err
	}
	if inf, e := r.Lstat("install.lock"); e == nil {
		if e = owned(inf, false); e != nil {
			return nil, e
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	s.lock, err = r.OpenFile("install.lock", os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(s.lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, errors.New("worker install: installation is busy")
	}
	return s, nil
}

// Execution eventually uses a pathname, not os.Root. Its ancestors must not
// allow another user to replace a verified directory after the root is closed.
// Root-owned sticky directories permit private user-owned children (e.g. /tmp).
func secureAncestors(path string) error {
	part := string(filepath.Separator)
	for _, name := range append([]string{""}, strings.Split(strings.TrimPrefix(path, part), part)...) {
		if name != "" {
			part = filepath.Join(part, name)
		}
		i, err := os.Lstat(part)
		if err != nil {
			return err
		}
		st, ok := i.Sys().(*syscall.Stat_t)
		if !ok || !i.IsDir() || st.Uid != 0 && st.Uid != uint32(os.Geteuid()) || i.Mode().Perm()&0o022 != 0 && !(st.Uid == 0 && i.Mode()&os.ModeSticky != 0) {
			return errors.New("worker install: installation ancestor can be replaced by another user")
		}
	}
	return nil
}

func (s *store) close() {
	if s.lock != nil {
		s.lock.Close()
	}
	if s.root != nil {
		s.root.Close()
	}
}
func (s *store) dir(name string, create bool) error {
	if create {
		if e := s.root.Mkdir(name, 0o700); e != nil && !errors.Is(e, os.ErrExist) {
			return e
		}
	}
	i, e := s.root.Lstat(name)
	if e != nil {
		return e
	}
	return owned(i, true)
}
func (s *store) read(name string, limit int64) ([]byte, error) {
	i, err := s.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if err = owned(i, false); err != nil {
		return nil, err
	}
	f, err := s.root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(i, opened) || opened.Size() > limit {
		return nil, errors.New("worker install: invalid file identity or size")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, errors.New("worker install: bounded read failed")
	}
	return b, nil
}
func (s *store) newFile(name string, b []byte, mode os.FileMode) error {
	f, err := s.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	return err
}
func (s *store) sync(name string) error {
	f, err := s.root.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func scratch() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return ".install-" + hex.EncodeToString(b[:])
}
func binaryName(m Manifest, a Artifact) string { return "versions/" + m.Version + "-" + a.SHA256 }

// Install publishes a verified binary exclusively, then replaces only our
// selector atomically. No old version, host, hook, or session record is removed.
// Partial input and every refusal before selector commit leave it unchanged.
func Install(path string, req Request) (Result, error) {
	m, a, err := Verify(req.Manifest, req.Signature, req.Trusted, req.Platform, req.Protocol)
	if err != nil {
		return Result{}, err
	}
	if req.Binary == nil {
		return Result{}, errors.New("worker install: missing binary")
	}
	s, err := openStore(path, req.Trusted, true)
	if err != nil {
		return Result{}, err
	}
	defer s.close()
	// Persist the dedicated prefix's directory entry before committing a
	// selector inside it, including on the first installation.
	parent, err := os.Open(filepath.Dir(s.root.Name()))
	if err != nil {
		return Result{}, err
	}
	err = parent.Sync()
	if closeErr := parent.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return Result{}, err
	}
	if _, err = s.selected(req.Platform, req.Protocol); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Result{}, err
	}
	temp := scratch()
	f, err := s.root.OpenFile(temp, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Result{}, err
	}
	defer s.root.Remove(temp) // Only the scratch inode created by this call.
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(req.Binary, a.Size+1))
	if err != nil || n != a.Size || hex.EncodeToString(h.Sum(nil)) != a.SHA256 {
		return Result{}, errors.New("worker install: incomplete binary or checksum mismatch")
	}
	if err = checkBinary(f, req.Platform); err != nil {
		return Result{}, err
	}
	if err = f.Chmod(0o500); err == nil {
		err = f.Sync()
	}
	if err != nil {
		return Result{}, err
	}
	// A published executable must have no installer write descriptor left
	// open, or a simultaneous explicit launch can fail with ETXTBSY.
	if err = f.Close(); err != nil {
		return Result{}, err
	}
	name := binaryName(m, a)
	if err = s.root.Link(temp, name); errors.Is(err, os.ErrExist) {
		err = s.verifyBinary(name, a)
	}
	if err != nil {
		return Result{}, err
	}
	if err = s.sync("versions"); err != nil {
		return Result{}, err
	}
	selection := Selection{Manifest: req.Manifest, Signature: req.Signature, Platform: req.Platform}
	b, err := json.Marshal(selection)
	if err != nil {
		return Result{}, err
	}
	selectTemp := scratch()
	if err = s.newFile(selectTemp, b, 0o400); err != nil {
		return Result{}, err
	}
	defer s.root.Remove(selectTemp)
	if err = s.root.Rename(selectTemp, "selected.json"); err != nil {
		return Result{}, err
	}
	result := Result{Path: filepath.Join(s.path, name), Prefix: s.path, Version: m.Version}
	if err = s.sync("."); err != nil {
		result.DurabilityWarning = "selection committed; directory durability could not be confirmed: " + err.Error()
	}
	return result, nil
}

func (s *store) verifyBinary(name string, a Artifact) error {
	i, err := s.root.Lstat(name)
	if err != nil {
		return err
	}
	if err = owned(i, false); err != nil {
		return err
	}
	if i.Size() != a.Size || i.Mode().Perm() != 0o500 {
		return errors.New("worker install: installed binary has unexpected size or mode")
	}
	f, err := s.root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(i, opened) {
		return errors.New("worker install: installed binary identity changed")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, a.Size+1))
	if err != nil || n != a.Size || hex.EncodeToString(h.Sum(nil)) != a.SHA256 {
		return errors.New("worker install: installed binary checksum mismatch")
	}
	return checkBinary(f, a.Platform)
}

func (s *store) selected(platform Platform, protocol string) (Result, error) {
	b, err := s.read("selected.json", 2*MaxManifest)
	if err != nil {
		return Result{}, err
	}
	var sel Selection
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(&sel); err != nil || d.Decode(new(any)) != io.EOF || sel.Platform != platform {
		return Result{}, errors.New("worker install: invalid selection")
	}
	m, a, err := Verify(sel.Manifest, sel.Signature, s.key, platform, protocol)
	if err != nil {
		return Result{}, err
	}
	name := binaryName(m, a)
	if err = s.verifyBinary(name, a); err != nil {
		return Result{}, fmt.Errorf("worker install: selection unavailable: %v", err)
	}
	return Result{Path: filepath.Join(s.path, name), Prefix: s.path, Version: m.Version}, nil
}

// Selected resolves and re-verifies the immutable executable for an explicit
// future launch. Existing hosts retain their original executable and hook paths.
func Selected(path string, platform Platform, protocol string) (Result, error) {
	s, err := openStore(path, nil, false)
	if err != nil {
		return Result{}, err
	}
	defer s.close()
	return s.selected(platform, protocol)
}
