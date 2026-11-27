package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/yousan/hataori/internal/registry"
)

const LocalMachine = "local"

// ErrMachineBinding refuses to adopt threads from a different installation.
var ErrMachineBinding = errors.New("machine binding differs from its recorded identity")

// MachineBinding is durable even when the machine is removed from configuration.
// Configured means listed, not reachable, healthy or eligible for new work.
type MachineBinding struct {
	Key, SSH, Root, Installation string
	Configured                   bool
}

// SyncMachines records configuration atomically. Removing a key makes it
// unavailable; it never deletes its binding or changes any thread's machine.
// Installation is pinned separately, by a future verified worker handshake.
func (s *Store) SyncMachines(ctx context.Context, configured []MachineBinding) error {
	return s.Update(ctx, func(tx *Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE machines SET configured = (key = 'local')`); err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, m := range configured {
			if err := registry.ValidName(m.Key); err != nil {
				return err
			}
			if seen[m.Key] {
				return fmt.Errorf("store: duplicate machine key %q", m.Key)
			}
			seen[m.Key] = true
			if m.Installation != "" {
				return fmt.Errorf("store: machine %q: installation is pinned by the handshake, not configuration", m.Key)
			}
			if m.Key == LocalMachine && (m.SSH != "" || m.Root != "") {
				return fmt.Errorf("store: machine %q: %w", m.Key, ErrMachineBinding)
			}
			if m.Key != LocalMachine && (m.SSH == "" || m.Root == "") {
				return fmt.Errorf("store: machine %q needs a destination and root", m.Key)
			}
			var ssh, root string
			err := tx.QueryRowContext(ctx, `SELECT ssh, root FROM machines WHERE key = ?`, m.Key).Scan(&ssh, &root)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				if _, err := tx.ExecContext(ctx, `INSERT INTO machines (key, ssh, root, configured) VALUES (?, ?, ?, 1)`, m.Key, m.SSH, m.Root); err != nil {
					return err
				}
			case err != nil:
				return err
			case ssh != m.SSH || root != m.Root:
				return fmt.Errorf("store: machine %q: %w; restore the original binding or resolve it explicitly", m.Key, ErrMachineBinding)
			default:
				if _, err := tx.ExecContext(ctx, `UPDATE machines SET configured = 1 WHERE key = ?`, m.Key); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// Machines returns all identities, including unavailable removed machines.
func (s *Store) Machines(ctx context.Context) ([]MachineBinding, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT key, ssh, root, installation, configured FROM machines ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MachineBinding
	for rows.Next() {
		var m MachineBinding
		if err := rows.Scan(&m.Key, &m.SSH, &m.Root, &m.Installation, &m.Configured); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// BindMachineInstallation pins the verified installation identity on first
// contact, and refuses a different one, including after config removal/re-add.
// It does not mark a worker healthy or authorize any operation.
func (s *Store) BindMachineInstallation(ctx context.Context, key, installation string) error {
	if key == LocalMachine || installation == "" {
		return fmt.Errorf("store: machine %q: invalid worker installation binding", key)
	}
	return s.Update(ctx, func(tx *Tx) error {
		var pinned string
		var configured bool
		if err := tx.QueryRowContext(ctx, `SELECT installation, configured FROM machines WHERE key = ?`, key).Scan(&pinned, &configured); err != nil {
			return err
		}
		if !configured {
			return fmt.Errorf("store: machine %q is not configured", key)
		}
		if pinned != "" && pinned != installation {
			return fmt.Errorf("store: machine %q: %w", key, ErrMachineBinding)
		}
		_, err := tx.ExecContext(ctx, `UPDATE machines SET installation = ? WHERE key = ?`, installation, key)
		return err
	})
}

// PendingPlacement is an accepted ordinary start waiting under the same thread
// number. Machine is empty for automatic placement; no session is launched here.
type PendingPlacement struct {
	Thread                       int64
	Machine, Agent, Data, Reason string
	CreatedAt                    int64
}

// PutPendingPlacement is safe to repeat with the original accepted request.
// A different request cannot replace it. Use the same transaction as the inbox
// acceptance and thread creation so a restart cannot make a second thread.
func (tx *Tx) PutPendingPlacement(ctx context.Context, p PendingPlacement) error {
	var confined bool
	var kind string
	var placed sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT confined, kind, machine FROM threads WHERE id = ?`, p.Thread).Scan(&confined, &kind, &placed); err != nil {
		return err
	}
	if confined || kind != "ordinary" {
		return errors.New("store: confined threads cannot wait for machine placement")
	}
	if p.Agent == "" || p.Data == "" {
		return errors.New("store: pending placement needs an agent and accepted request")
	}
	if placed.Valid && p.Machine != "" && placed.String != p.Machine {
		return errors.New("store: pending request contradicts the thread's machine")
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO pending_placements (thread_id, machine, agent, data, created_at, reason)
		VALUES (?, NULLIF(?, ''), ?, ?, ?, ?) ON CONFLICT(thread_id) DO NOTHING`, p.Thread, p.Machine, p.Agent, p.Data, p.CreatedAt, p.Reason)
	if err != nil {
		return err
	}
	var machine, agent, data string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(machine, ''), agent, data FROM pending_placements WHERE thread_id = ?`, p.Thread).Scan(&machine, &agent, &data); err != nil {
		return err
	}
	if machine != p.Machine || agent != p.Agent || data != p.Data {
		return errors.New("store: pending placement already belongs to another request")
	}
	return nil
}

func (s *Store) PendingPlacements(ctx context.Context) ([]PendingPlacement, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT thread_id, COALESCE(machine, ''), agent, data, reason, created_at FROM pending_placements ORDER BY created_at, thread_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingPlacement
	for rows.Next() {
		var p PendingPlacement
		if err := rows.Scan(&p.Thread, &p.Machine, &p.Agent, &p.Data, &p.Reason, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// MachineReservation accounts for an externally chosen starting/resident slot.
// Operation identifies its original effect, so late reconciliation cannot
// consume a newer reservation. This storage API performs no placement policy.
type MachineReservation struct {
	Thread                    int64
	Machine, Agent, Operation string
	CreatedAt                 int64
}

func (tx *Tx) ReserveMachine(ctx context.Context, r MachineReservation) error {
	if r.Machine == "" || r.Agent == "" || r.Operation == "" {
		return errors.New("store: reservation needs machine, agent and operation")
	}
	var configured bool
	if err := tx.QueryRowContext(ctx, `SELECT configured FROM machines WHERE key = ?`, r.Machine).Scan(&configured); err != nil {
		return err
	}
	if !configured {
		return fmt.Errorf("store: machine %q is not configured", r.Machine)
	}
	// A reservation may reconcile an existing session with no pending start.
	// When a start is pending, its accepted agent and explicit machine are
	// immutable request data, independent of the later placement policy.
	var requestedMachine, requestedAgent string
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(machine, ''), agent FROM pending_placements WHERE thread_id = ?`, r.Thread).Scan(&requestedMachine, &requestedAgent)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && (requestedAgent != r.Agent || requestedMachine != "" && requestedMachine != r.Machine) {
		return errors.New("store: reservation contradicts the accepted placement request")
	}
	// Bind once, for a thread whose initial machine choice was pending.
	if _, err := tx.ExecContext(ctx, `UPDATE threads SET machine = ? WHERE id = ? AND machine IS NULL`, r.Machine, r.Thread); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO machine_reservations (thread_id, machine, agent, operation, created_at)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT(thread_id) DO NOTHING`, r.Thread, r.Machine, r.Agent, r.Operation, r.CreatedAt); err != nil {
		return err
	}
	var machine, agent, operation string
	if err := tx.QueryRowContext(ctx, `SELECT machine, agent, operation FROM machine_reservations WHERE thread_id = ?`, r.Thread).Scan(&machine, &agent, &operation); err != nil {
		return err
	}
	if machine != r.Machine || agent != r.Agent || operation != r.Operation {
		return errors.New("store: reservation already belongs to another operation")
	}
	return nil
}

func (s *Store) MachineReservations(ctx context.Context) ([]MachineReservation, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT thread_id, machine, agent, operation, created_at FROM machine_reservations ORDER BY created_at, thread_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MachineReservation
	for rows.Next() {
		var r MachineReservation
		if err := rows.Scan(&r.Thread, &r.Machine, &r.Agent, &r.Operation, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// FinishPlacement reconciles the named reservation on the existing thread.
// Call it with launch/receipt state changes in the same transaction. It neither
// starts nor retries an effect and refuses stale operation replies.
func (tx *Tx) FinishPlacement(ctx context.Context, thread int64, operation string) error {
	res, err := tx.ExecContext(ctx, `DELETE FROM machine_reservations WHERE thread_id = ? AND operation = ?`, thread, operation)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("store: no matching placement reservation")
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM pending_placements WHERE thread_id = ?`, thread)
	return err
}
