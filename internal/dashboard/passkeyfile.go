package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"
)

// PasskeysFile is where the dashboard keeps the passkeys, under the
// instance root, next to its session key. Removing it removes every
// passkey.
const PasskeysFile = "state/dashboard-passkeys.json"

// maxPasskeysFile is the most of the file that is read.
const maxPasskeysFile = 1 << 20

// passkey is one registered passkey: what verifies its signatures, and
// whose it is.
type passkey struct {
	ID     []byte `json:"id"`     // the credential id
	Person string `json:"person"` // whose session it opens
	// Handle is the user handle it was made with, which the authenticator
	// returns with each signature: one per person, the same for all of
	// that person's passkeys.
	Handle    []byte `json:"user_handle"`
	PublicKey []byte `json:"public_key"` // COSE_Key
	SignCount uint32 `json:"sign_count"` // the signature counter last seen; 0 when the authenticator keeps none
	// BackupEligible is whether the passkey may be synced, which may not
	// change; BackupState whether it was synced when last checked.
	BackupEligible bool       `json:"backup_eligible"`
	BackupState    bool       `json:"backup_state"`
	Made           time.Time  `json:"made"`
	Used           *time.Time `json:"used,omitempty"` // the last sign-in with it
}

// passkeysOnDisk is the file's content.
type passkeysOnDisk struct {
	Version  int       `json:"version"`
	Passkeys []passkey `json:"passkeys"`
}

// passkeyFile is the passkeys file of one instance. It is read for every
// change and written whole, under another name and then renamed, so that
// a crash leaves the old file or the new one and never part of either.
// The daemon holds the instance lock, so it is the only writer.
type passkeyFile struct {
	root *os.Root
	mu   sync.Mutex // one change at a time
}

// load reads the passkeys; none when there is no file. Whoever may write
// the file may add a passkey, so one that others may open is refused, as
// the session key is.
func (f *passkeyFile) load() ([]passkey, error) {
	h, err := f.root.OpenFile(PasskeysFile, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("dashboard: %s: %w", PasskeysFile, err)
	}
	defer h.Close()
	st, err := h.Stat()
	if err != nil {
		return nil, fmt.Errorf("dashboard: %s: %w", PasskeysFile, err)
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("dashboard: %s is not a regular file", PasskeysFile)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("dashboard: %s is accessible to other users (mode %04o); run chmod 600 on it, or remove it to remove every passkey", PasskeysFile, st.Mode().Perm())
	}
	b, err := io.ReadAll(io.LimitReader(h, maxPasskeysFile+1))
	if err != nil {
		return nil, fmt.Errorf("dashboard: %s: %w", PasskeysFile, err)
	}
	if len(b) > maxPasskeysFile {
		return nil, fmt.Errorf("dashboard: %s is larger than %d bytes", PasskeysFile, maxPasskeysFile)
	}
	var d passkeysOnDisk
	if err := json.Unmarshal(b, &d); err != nil || d.Version != 1 {
		return nil, fmt.Errorf("dashboard: %s is not a passkeys file of this version (%v): remove it to remove every passkey", PasskeysFile, err)
	}
	return d.Passkeys, nil
}

// save writes keys as the whole file.
func (f *passkeyFile) save(keys []passkey) error {
	if keys == nil {
		keys = []passkey{}
	}
	b, err := json.MarshalIndent(passkeysOnDisk{Version: 1, Passkeys: keys}, "", "  ")
	if err != nil {
		return fmt.Errorf("dashboard: %w", err)
	}
	tmp := PasskeysFile + ".new"
	if err := f.root.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("dashboard: %w", err)
	}
	h, err := f.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("dashboard: %w", err)
	}
	_, err = h.Write(append(b, '\n'))
	if err == nil {
		err = h.Sync()
	}
	if cerr := h.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = f.root.Rename(tmp, PasskeysFile)
	}
	if err != nil {
		f.root.Remove(tmp)
		return fmt.Errorf("dashboard: %w", err)
	}
	return nil
}

// change reads the passkeys, gives them to fn, and writes what fn returns
// when it says to, all under one lock. Nothing is written when fn fails.
func (f *passkeyFile) change(fn func(keys []passkey) (out []passkey, write bool, err error)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	keys, err := f.load()
	if err != nil {
		return err
	}
	out, write, err := fn(keys)
	if err != nil || !write {
		return err
	}
	return f.save(out)
}

// list reads the passkeys under the lock, so that it sees no change half
// made.
func (f *passkeyFile) list() ([]passkey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.load()
}
