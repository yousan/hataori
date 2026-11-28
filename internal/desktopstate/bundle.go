package desktopstate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"syscall"

	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/nativehttp"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/wire"
)

type Manifest struct {
	Format          int               `json:"format"`
	Version         string            `json:"version"`
	Commit          string            `json:"commit"`
	Platform        string            `json:"platform"`
	Arch            string            `json:"arch"`
	Schema          int               `json:"schema"`
	ControlProtocol string            `json:"control_protocol"`
	HostCapability  string            `json:"host_capability"`
	Files           map[string]string `json:"files"`
}

var bundleVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-dev\.[0-9a-f]{40})?$`)
var commitID = regexp.MustCompile(`^[0-9a-f]{40}$`)
var bundleFiles = []string{"hataori", "hataori-desktop-transport"}

func architecture() string {
	if runtime.GOARCH == "amd64" {
		return "x64"
	}
	return runtime.GOARCH
}

func readManifest(reader io.Reader) (Manifest, error) {
	var manifest Manifest
	d := json.NewDecoder(io.LimitReader(reader, 16385))
	d.DisallowUnknownFields()
	if err := d.Decode(&manifest); err != nil {
		return manifest, err
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF || manifest.Format != 1 || !bundleVersion.MatchString(manifest.Version) || !commitID.MatchString(manifest.Commit) || manifest.Platform != "darwin" || manifest.Arch != architecture() || len(manifest.Files) != len(bundleFiles) || manifest.Schema < 1 || manifest.HostCapability != host.InputBatchVersion || strings.Split(manifest.ControlProtocol, ".")[0] != strings.Split(wire.Version, ".")[0] {
		return manifest, errors.New("desktop: incompatible bundle manifest")
	}
	for _, name := range bundleFiles {
		digest, err := hex.DecodeString(manifest.Files[name])
		if err != nil || len(digest) != sha256.Size {
			return manifest, errors.New("desktop: invalid executable checksum")
		}
	}
	return manifest, nil
}

func digestFile(file *os.File, expected string) error {
	info, err := file.Stat()
	if err != nil || info.Size() > 512*1024*1024 {
		return errors.New("desktop: invalid executable size")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != expected {
		return errors.New("desktop: executable checksum mismatch")
	}
	_, err = file.Seek(0, io.SeekStart)
	return err
}

// Stage checks the matching app's resources before creating a new versioned
// directory. Existing files, partial stages and referenced binaries are never
// overwritten or removed. A failed stage cannot change persisted intent or the
// active selection. initial app-resource trust comes from the maintainer's
// chosen unpublished tagged distribution; checksums are not a signing claim.
func (s *State) Stage(ctx context.Context, resources, expectedVersion string) (Selection, error) {
	if err := ctx.Err(); err != nil {
		return Selection{}, err
	}
	manifestFile, err := os.Open(filepath.Join(resources, "desktop-manifest.json"))
	if err != nil {
		return Selection{}, err
	}
	manifest, err := readManifest(manifestFile)
	manifestFile.Close()
	if err != nil || manifest.Version != expectedVersion || manifest.Schema != store.SchemaVersion() {
		return Selection{}, errors.New("desktop: bundle does not match this daemon build")
	}
	sources := make(map[string]*os.File)
	defer func() {
		for _, source := range sources {
			source.Close()
		}
	}()
	for _, name := range bundleFiles {
		path := filepath.Join(resources, name)
		before, err := os.Lstat(path)
		if err != nil {
			return Selection{}, err
		}
		stat, ok := before.Sys().(*syscall.Stat_t)
		if !ok || !before.Mode().IsRegular() || before.Mode().Perm()&0o022 != 0 || before.Mode().Perm()&0o111 == 0 || stat.Nlink != 1 || stat.Uid != 0 && int(stat.Uid) != os.Geteuid() {
			return Selection{}, errors.New("desktop: untrusted app executable")
		}
		file, err := os.Open(path)
		if err != nil {
			return Selection{}, err
		}
		sources[name] = file
		after, err := file.Stat()
		if err != nil || !os.SameFile(before, after) || digestFile(file, manifest.Files[name]) != nil {
			return Selection{}, errors.New("desktop: invalid app executable")
		}
	}
	if err := RefuseCollision(s.root.Name(), "bin"); err != nil {
		return Selection{}, err
	}
	bin, err := nativehttp.OpenPrivateDir(s.root, "bin", true)
	if err != nil {
		return Selection{}, err
	}
	defer bin.Close()
	if err := RefuseCollision(bin.Name(), manifest.Version); err != nil {
		return Selection{}, err
	}
	selection := Selection{Intent: Stopped, Version: manifest.Version, Binary: filepath.Join(bin.Name(), manifest.Version, "hataori")}
	if _, err := bin.Lstat(manifest.Version); err == nil {
		// Idempotence requires a complete already-verified immutable stage.
		if err := s.VerifySelection(ctx, selection); err != nil {
			return Selection{}, err
		}
		f, err := os.Open(filepath.Join(bin.Name(), manifest.Version, "desktop-manifest.json"))
		if err != nil {
			return Selection{}, err
		}
		previous, err := readManifest(f)
		f.Close()
		if err != nil || !reflect.DeepEqual(previous, manifest) {
			return Selection{}, errors.New("desktop: version already names a different bundle")
		}
		return selection, nil
	} else if !os.IsNotExist(err) {
		return Selection{}, err
	}
	target, err := nativehttp.OpenPrivateDir(bin, manifest.Version, true)
	if err != nil {
		return Selection{}, err
	}
	defer target.Close()
	for _, name := range bundleFiles {
		file, err := nativehttp.OpenPrivateFile(target, name, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
		if err != nil {
			return Selection{}, err
		}
		_, copyErr := io.Copy(file, sources[name])
		err = errors.Join(copyErr, file.Sync())
		if err == nil {
			err = file.Chmod(0o700)
		}
		file.Close()
		if err != nil {
			return Selection{}, err
		}
	}
	file, err := nativehttp.OpenPrivateFile(target, "desktop-manifest.json", os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return Selection{}, err
	}
	err = json.NewEncoder(file).Encode(manifest)
	if err == nil {
		err = file.Sync()
	}
	file.Close()
	if err != nil {
		return Selection{}, err
	}
	return selection, s.VerifySelection(ctx, selection)
}

// VerifySelection is required by register/select and again immediately before
// each child start. A stored pathname never grants process-start authority.
func (s *State) VerifySelection(ctx context.Context, selection Selection) error {
	if err := validSelection(selection); err != nil {
		return err
	}
	if !bundleVersion.MatchString(selection.Version) || selection.Binary != filepath.Join(s.root.Name(), "bin", selection.Version, "hataori") {
		return errors.New("desktop: daemon is outside its private version directory")
	}
	if err := RefuseCollision(s.root.Name(), "bin"); err != nil {
		return err
	}
	bin, err := nativehttp.OpenPrivateDir(s.root, "bin", false)
	if err != nil {
		return err
	}
	defer bin.Close()
	if err := RefuseCollision(bin.Name(), selection.Version); err != nil {
		return err
	}
	target, err := nativehttp.OpenPrivateDir(bin, selection.Version, false)
	if err != nil {
		return err
	}
	defer target.Close()
	for _, name := range append([]string{"desktop-manifest.json"}, bundleFiles...) {
		if err := RefuseCollision(target.Name(), name); err != nil {
			return err
		}
	}
	file, err := nativehttp.OpenPrivateFile(target, "desktop-manifest.json", os.O_RDONLY)
	if err != nil {
		return err
	}
	manifest, err := readManifest(file)
	file.Close()
	if err != nil || manifest.Version != selection.Version {
		return errors.New("desktop: selected bundle manifest is incompatible")
	}
	for _, name := range bundleFiles {
		file, err := nativehttp.OpenPrivateExecutable(target, name)
		if err != nil {
			return err
		}
		err = digestFile(file, manifest.Files[name])
		file.Close()
		if err != nil {
			return err
		}
	}
	database, err := nativehttp.OpenPrivateDatabase(s.state, "hataori.db")
	if err == nil {
		database.Close()
		current, err := store.ReadSchemaVersion(ctx, s.root.Name())
		if err != nil || current > manifest.Schema {
			return errors.New("desktop: persisted schema is incompatible; rollback refused")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}
