// Package desktopsetup validates and privately commits first-run input while
// the daemon's normal chat/thread/terminal operations are unavailable.
package desktopsetup

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/yousan/hataori/internal/desktopstate"
	"github.com/yousan/hataori/internal/nativehttp"
)

type Agent struct {
	Name    string `json:"name"`
	Command string `json:"command"`
}

type Info struct {
	Existing bool    `json:"existing"`
	Agents   []Agent `json:"agents"`
}

type Input struct {
	Owner        string `json:"owner"`
	OwnerID      string `json:"owner_id"`
	BotID        string `json:"bot_id"`
	Token        string `json:"token"`
	Scope        string `json:"scope"`
	Repository   string `json:"repository"`
	Agent        string `json:"agent"`
	StartAtLogin bool   `json:"start_at_login"`
}

// Build is supplied by cmd, where adapter-specific configuration belongs.
// Validate is the existing daemon configuration parser, not a second schema.
type Build func(Input, Agent, string) ([]byte, error)
type Validate func([]byte) error

type Setup struct {
	root, state, config *os.Root
	lease               *os.File
	info                Info
	build               Build
	validate            Validate
	mu                  sync.Mutex
	complete            bool
	Done                chan struct{}
}

// CreateRoot makes only missing private directories, refusing differently
// spelled case/Unicode aliases. Nothing existing is repaired or overwritten.
func CreateRoot(path string) (*os.Root, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	parent := filepath.Dir(abs)
	if _, err := os.Lstat(parent); os.IsNotExist(err) {
		p, err := CreateRoot(parent)
		if err != nil {
			return nil, err
		}
		p.Close()
	} else if err != nil {
		return nil, err
	}
	if err := desktopstate.RefuseCollision(parent, filepath.Base(abs)); err != nil {
		return nil, err
	}
	return nativehttp.CreatePrivateLeaf(abs)
}

// Open holds the same permanent state/lock used by the daemon. A busy or
// incompatible root is an error, never permission to delete/steal that lock.
func Open(path string, agents []Agent, build Build, validate Validate) (_ *Setup, err error) {
	s := &Setup{build: build, validate: validate, info: Info{Agents: agents}, Done: make(chan struct{})}
	defer func() {
		if err != nil {
			s.Close()
		}
	}()
	s.root, err = CreateRoot(path)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"state", "config"} {
		if err = desktopstate.RefuseCollision(s.root.Name(), name); err != nil {
			return nil, err
		}
	}
	s.state, err = nativehttp.OpenPrivateDir(s.root, "state", true)
	if err != nil {
		return nil, err
	}
	s.lease, err = nativehttp.OpenPrivateFile(s.state, "lock", os.O_RDWR|os.O_CREATE)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(s.lease.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.New("desktop setup: the selected instance is already locked")
	}
	s.config, err = nativehttp.OpenPrivateDir(s.root, "config", true)
	if err != nil {
		return nil, err
	}
	if err = desktopstate.RefuseCollision(s.config.Name(), "hataori.json"); err != nil {
		return nil, err
	}
	f, err := nativehttp.OpenPrivateFile(s.config, "hataori.json", os.O_RDONLY)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 1024*1024+1))
	if err != nil || len(data) > 1024*1024 || validate(data) != nil {
		return nil, errors.New("desktop setup: existing configuration is invalid; it was left intact")
	}
	s.info.Existing = true
	return s, nil
}

func (s *Setup) Root() string { return s.root.Name() }
func (s *Setup) Info() Info   { return s.info }

// Commit never overwrites an existing config, token or login. The selected
// agent comes from GUI-side executable discovery. Tokens go only into an
// exclusive 0600 file and never a command line, response, stdout or log.
func (s *Setup) Commit(_ context.Context, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.complete {
		return errors.New("desktop setup: already committed")
	}
	var input Input
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&input) != nil || !input.StartAtLogin {
		return errors.New("desktop setup: explicit login-start registration is required")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("desktop setup: unexpected input")
	}
	if !s.info.Existing {
		var selected Agent
		for _, a := range s.info.Agents {
			if a.Name == input.Agent {
				selected = a
			}
		}
		if selected.Command == "" || len(input.Token) == 0 || len(input.Token) > 8192 || strings.ContainsAny(input.Token, "\x00\r\n") {
			return errors.New("desktop setup: missing agent or invalid token")
		}
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return err
		}
		name := "desktop-token-" + hex.EncodeToString(random[:])
		config, err := s.build(input, selected, name)
		if err != nil || s.validate(config) != nil {
			return errors.New("desktop setup: configuration validation failed")
		}
		if err := s.writeNew(name, []byte(input.Token)); err != nil {
			return err
		}
		// The root lock prevents a daemon reading a half-written config.
		// A failed/crashed write is retained and refused on reopen; it is
		// never repaired by overwriting a possibly valuable existing file.
		if err := s.writeNew("hataori.json", config); err != nil {
			return err
		}
	}
	s.complete = true
	return nil
}

// Published is called by the HTTP handler only after flushing success. The
// owner command then closes/joins the restricted listener before releasing
// the root lock and beginning ordinary daemon startup.
func (s *Setup) Published() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.complete {
		select {
		case <-s.Done:
		default:
			close(s.Done)
		}
	}
}

func (s *Setup) writeNew(name string, data []byte) error {
	f, err := nativehttp.OpenPrivateFile(s.config, name, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return errors.New("desktop setup: an existing private file was preserved")
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return errors.New("desktop setup: private write failed; partial file retained")
	}
	return f.Sync()
}

func (s *Setup) Close() {
	if s.lease != nil {
		s.lease.Close()
	}
	for _, root := range []*os.Root{s.config, s.state, s.root} {
		if root != nil {
			root.Close()
		}
	}
}
