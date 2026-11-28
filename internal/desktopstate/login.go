package desktopstate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yousan/hataori/internal/nativehttp"
)

type Registration struct {
	Label string `json:"label"`
	Path  string `json:"path"`
	Root  string `json:"root"`
}

func (s *State) loginLabel() string {
	digest := sha256.Sum256([]byte(s.root.Name()))
	return "io.hataori.desktop." + hex.EncodeToString(digest[:16])
}

func xmlText(s string) string { var b bytes.Buffer; xml.EscapeText(&b, []byte(s)); return b.String() }

// LoginPlist contains fixed arguments only, no shell, tokens or agent login.
// AbandonProcessGroup is essential: launchd must leave detached session hosts
// alive when its launcher exits. Its lifetime pipe ends only the owned daemon.
func LoginPlist(label, root, binary string, env map[string]string) []byte {
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n<plist version=\"1.0\"><dict>\n")
	fmt.Fprintf(&b, "<key>Label</key><string>%s</string>\n<key>ProgramArguments</key><array>", xmlText(label))
	for _, arg := range []string{binary, "desktop", "launcher", "--root", root} {
		fmt.Fprintf(&b, "<string>%s</string>", xmlText(arg))
	}
	b.WriteString("</array>\n<key>RunAtLoad</key><true/><key>KeepAlive</key><true/><key>AbandonProcessGroup</key><true/><key>ThrottleInterval</key><integer>30</integer>\n<key>ProcessType</key><string>Background</string>\n<key>EnvironmentVariables</key><dict>")
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(&b, "<key>%s</key><string>%s</string>", xmlText(key), xmlText(env[key]))
	}
	b.WriteString("</dict>\n</dict></plist>\n")
	return []byte(b.String())
}

// InstallLogin creates a private registration once. An existing different
// file is preserved, including another version's valid launcher reference.
// No launchctl, home-directory discovery or service action happens here.
func (s *State) InstallLogin(ctx context.Context, directory string, env map[string]string) (Registration, error) {
	d, err := s.Read(ctx)
	if err != nil {
		return Registration{}, err
	}
	if err := s.VerifySelection(ctx, d); err != nil {
		return Registration{}, err
	}
	previous, err := s.ReadLogin(ctx)
	if err == nil {
		canonical, e := filepath.EvalSymlinks(directory)
		if e != nil || filepath.Dir(previous.Path) != canonical {
			return Registration{}, errors.New("desktop: login registration directory changed")
		}
		return previous, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Registration{}, err
	}
	for key, value := range env {
		switch key {
		case "HOME", "USER", "LOGNAME", "PATH", "TMPDIR", "LANG", "LC_ALL":
		default:
			return Registration{}, errors.New("desktop: unsafe login environment key")
		}
		if strings.ContainsAny(value, "\x00\r\n") || len(value) > 8192 {
			return Registration{}, errors.New("desktop: invalid login environment")
		}
	}
	if !filepath.IsAbs(env["HOME"]) || env["PATH"] == "" {
		return Registration{}, errors.New("desktop: incomplete GUI environment")
	}
	r, err := nativehttp.OpenOwnerDirectory(directory)
	if err != nil {
		return Registration{}, err
	}
	defer r.Close()
	reg := Registration{Label: s.loginLabel(), Root: s.root.Name()}
	name := reg.Label + ".plist"
	reg.Path = filepath.Join(r.Name(), name)
	if err := RefuseCollision(r.Name(), name); err != nil {
		return Registration{}, err
	}
	data := LoginPlist(reg.Label, reg.Root, d.Binary, env)
	f, err := nativehttp.OpenPrivateFile(r, name, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if errors.Is(err, os.ErrExist) {
		f, err = nativehttp.OpenPrivateFile(r, name, os.O_RDONLY)
		if err != nil {
			return Registration{}, err
		}
		defer f.Close()
		existing, err := io.ReadAll(io.LimitReader(f, 64*1024+1))
		if err != nil || !bytes.Equal(existing, data) {
			return Registration{}, errors.New("desktop: existing login registration was preserved; use its current launcher")
		}
		_, err = s.db.ExecContext(ctx, `INSERT INTO login(id,path,plist) VALUES(1,?,?)`, reg.Path, data)
		return reg, err
	}
	if err != nil {
		return Registration{}, err
	}
	err = errors.Join(func() error { _, e := f.Write(data); return e }(), f.Sync())
	f.Close()
	if err != nil {
		return Registration{}, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO login(id,path,plist) VALUES(1,?,?)`, reg.Path, data)
	return reg, err
}

// ReadLogin proves that an OS service action names this instance's exact,
// unchanged private registration. It never discovers a service by PID,
// command substring, process group or a renderer-provided label.
func (s *State) ReadLogin(ctx context.Context) (Registration, error) {
	reg := Registration{Label: s.loginLabel(), Root: s.root.Name()}
	var expected []byte
	if err := s.db.QueryRowContext(ctx, `SELECT path,plist FROM login WHERE id=1`).Scan(&reg.Path, &expected); err != nil {
		return Registration{}, err
	}
	if !filepath.IsAbs(reg.Path) || filepath.Clean(reg.Path) != reg.Path || filepath.Base(reg.Path) != reg.Label+".plist" || len(expected) > 64*1024 {
		return Registration{}, errors.New("desktop: invalid stored login registration")
	}
	r, err := nativehttp.OpenOwnerDirectory(filepath.Dir(reg.Path))
	if err != nil {
		return Registration{}, err
	}
	defer r.Close()
	if err := RefuseCollision(r.Name(), filepath.Base(reg.Path)); err != nil {
		return Registration{}, err
	}
	f, err := nativehttp.OpenPrivateFile(r, filepath.Base(reg.Path), os.O_RDONLY)
	if err != nil {
		return Registration{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 64*1024+1))
	if err != nil || !bytes.Equal(data, expected) {
		return Registration{}, errors.New("desktop: login registration changed; service action refused")
	}
	return reg, nil
}
