package store

import "context"

// Verified inventory is retained across transport failure. Only a complete,
// bound reconciliation replaces it; silence never releases resident room.
var placementMigration = Migration{Name: "placement inventory observations", Up: execAll(
	`CREATE TABLE machine_inventory (
	 machine TEXT NOT NULL REFERENCES machines(key),
	 thread_id INTEGER NOT NULL REFERENCES threads(id),
	 session TEXT NOT NULL, run TEXT NOT NULL, state TEXT NOT NULL,
	 PRIMARY KEY(machine,session,run)
	)`,
	`INSERT OR IGNORE INTO machine_reservations(thread_id,machine,agent,operation,created_at)
	 SELECT t.id,t.machine,s.agent,'legacy-placement-'||t.id,t.created_at FROM threads t JOIN sessions s ON s.id=(SELECT max(v.id) FROM sessions v WHERE v.thread_id=t.id)
	 WHERE t.machine IS NOT NULL AND (s.state IN ('creating','idle','running','waiting') OR s.spec!='')`,
)}

// MachineRunObservation is supplied only after a complete, pinned inventory
// has passed protocol and target verification. Transport silence never calls it.
type MachineRunObservation struct {
	Thread              int64
	Session, Run, State string
}

func (tx *Tx) ObserveMachineRuns(ctx context.Context, key string, runs []MachineRunObservation) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM machine_inventory WHERE machine=?`, key); err != nil {
		return err
	}
	for _, run := range runs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO machine_inventory(machine,thread_id,session,run,state) VALUES(?,?,?,?,?)`, key, run.Thread, run.Session, run.Run, run.State); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM machine_reservations WHERE machine=?
 AND EXISTS(SELECT 1 FROM sessions s JOIN session_machine_runs x ON x.session_id=s.id JOIN machine_inventory i ON i.machine=x.machine AND i.run=x.run AND i.session='s-'||s.id
 WHERE s.thread_id=machine_reservations.thread_id AND s.id=(SELECT max(v.id) FROM sessions v WHERE v.thread_id=s.thread_id) AND s.state IN ('asleep','closed','failed','removed') AND i.state IN ('ended','gone'))
 AND NOT EXISTS(SELECT 1 FROM machine_inventory i WHERE i.machine=machine_reservations.machine AND i.thread_id=machine_reservations.thread_id AND i.state NOT IN ('ended','gone'))`, key)
	return err
}
