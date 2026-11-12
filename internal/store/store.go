// Package store persists locks and the audit log in SQLite.
//
// Design notes:
//   - A lock's expiry is never stored; it is always computed as
//     locked_at + timeout_seconds using the *current* timeout, so changing the
//     timeout in the configuration takes effect immediately and a restart never
//     disturbs a lock.
//   - Every operation runs in a single transaction, and the pool is limited to
//     one connection, so concurrent commands are serialized and two users can
//     never both acquire the same lock.
//   - Every operation first releases any locks that are past their timeout (in
//     the same transaction), so correctness never depends on how often the
//     background loop runs; the loop only determines how quickly timeouts are
//     announced.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"mattermost-locker/internal/textfmt"
)

const schema = `
CREATE TABLE IF NOT EXISTS locks (
	name            TEXT PRIMARY KEY COLLATE NOCASE,
	position        INTEGER NOT NULL,
	timeout_seconds INTEGER NOT NULL,
	holder_id       TEXT    NOT NULL DEFAULT '',
	holder_name     TEXT    NOT NULL DEFAULT '',
	locked_at       INTEGER NOT NULL DEFAULT 0
);

-- No foreign key on lock_name: history intentionally outlives removed locks.
CREATE TABLE IF NOT EXISTS audit_log (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	lock_name      TEXT    NOT NULL COLLATE NOCASE,
	action         TEXT    NOT NULL,
	user_id        TEXT    NOT NULL DEFAULT '',
	user_name      TEXT    NOT NULL DEFAULT '',
	prev_user_id   TEXT    NOT NULL DEFAULT '',
	prev_user_name TEXT    NOT NULL DEFAULT '',
	held_seconds   INTEGER NOT NULL DEFAULT 0,
	detail         TEXT    NOT NULL DEFAULT '',
	at             INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_audit_lock ON audit_log (lock_name, id);
`

const lockCols = "name, position, timeout_seconds, holder_id, holder_name, locked_at"

// Store is the SQLite-backed lock store.
type Store struct {
	db       *sqlx.DB
	now      func() time.Time
	onExpire func([]Expiry)
}

// Option customizes a Store.
type Option func(*Store)

// WithClock overrides the time source (used by tests).
func WithClock(now func() time.Time) Option { return func(s *Store) { s.now = now } }

// Open opens (creating if necessary) the database at path and applies the schema.
func Open(path string, opts ...Option) (*Store, error) {
	db, err := sqlx.Open(driverName, dsn(path))
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}
	// One connection: serializes all access (see package comment).
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("connecting to database %q: %w", path, err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("applying schema: %w", err)
	}

	s := &Store{db: db, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// SetExpiryHook registers a function called (after the transaction commits)
// whenever locks are released because they timed out. Set it before use.
func (s *Store) SetExpiryHook(fn func([]Expiry)) { s.onExpire = fn }

// tx runs fn in a transaction. When expire is true, locks past their timeout
// are released first, in the same transaction, and the expiry hook is invoked
// after a successful commit.
func (s *Store) tx(ctx context.Context, expire bool, fn func(tx *sqlx.Tx, now time.Time) error) error {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	now := s.now()

	var expired []Expiry
	if expire {
		if expired, err = expireDue(ctx, tx, now); err != nil {
			return err
		}
	}
	if fn != nil {
		if err := fn(tx, now); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	if len(expired) > 0 && s.onExpire != nil {
		s.onExpire(expired)
	}
	return nil
}

// ---- queries ----

func getLock(ctx context.Context, tx *sqlx.Tx, name string) (Lock, error) {
	var r lockRow
	err := tx.GetContext(ctx, &r, "SELECT "+lockCols+" FROM locks WHERE name = ?", name)
	if errors.Is(err, sql.ErrNoRows) {
		return Lock{}, ErrNotFound
	}
	if err != nil {
		return Lock{}, err
	}
	return r.lock(), nil
}

func insertAudit(ctx context.Context, tx *sqlx.Tx, e AuditEntry, at time.Time) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO audit_log
			(lock_name, action, user_id, user_name, prev_user_id, prev_user_name, held_seconds, detail, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.LockName, string(e.Action), e.UserID, e.UserName, e.PrevUserID, e.PrevUserName,
		int64(e.HeldFor/time.Second), e.Detail, at.Unix())
	return err
}

func setHolder(ctx context.Context, tx *sqlx.Tx, l *Lock, u User, now time.Time) error {
	if _, err := tx.ExecContext(ctx,
		"UPDATE locks SET holder_id = ?, holder_name = ?, locked_at = ? WHERE name = ?",
		u.ID, u.Name, now.Unix(), l.Name); err != nil {
		return err
	}
	l.HolderID, l.HolderName, l.LockedAt = u.ID, u.Name, time.Unix(now.Unix(), 0)
	return nil
}

func clearHolder(ctx context.Context, tx *sqlx.Tx, name string) error {
	_, err := tx.ExecContext(ctx,
		"UPDATE locks SET holder_id = '', holder_name = '', locked_at = 0 WHERE name = ?", name)
	return err
}

// expireDue releases every lock that has passed its timeout and records it.
func expireDue(ctx context.Context, tx *sqlx.Tx, now time.Time) ([]Expiry, error) {
	var rows []lockRow
	err := tx.SelectContext(ctx, &rows, `
		SELECT `+lockCols+` FROM locks
		WHERE holder_id <> '' AND locked_at + timeout_seconds <= ?
		ORDER BY locked_at, position`, now.Unix())
	if err != nil {
		return nil, err
	}

	out := make([]Expiry, 0, len(rows))
	for _, r := range rows {
		l := r.lock()
		if err := clearHolder(ctx, tx, l.Name); err != nil {
			return nil, err
		}
		exp := Expiry{Lock: l, ExpiredAt: l.ExpiresAt()}
		err := insertAudit(ctx, tx, AuditEntry{
			LockName: l.Name,
			Action:   ActionTimeout,
			UserID:   l.HolderID,
			UserName: l.HolderName,
			HeldFor:  l.Timeout,
		}, exp.ExpiredAt) // record when it actually expired, even if we were offline
		if err != nil {
			return nil, err
		}
		out = append(out, exp)
	}
	return out, nil
}

// ---- public operations ----

// ExpireDue releases locks that have timed out and fires the expiry hook.
func (s *Store) ExpireDue(ctx context.Context) error {
	return s.tx(ctx, true, nil)
}

// RunExpiryLoop calls ExpireDue every interval until ctx is cancelled.
func (s *Store) RunExpiryLoop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.ExpireDue(ctx); err != nil && ctx.Err() == nil {
				slog.Error("Expiring locks failed", "error", err)
			}
		}
	}
}

// List returns all locks in configuration order.
func (s *Store) List(ctx context.Context) ([]Lock, error) {
	var out []Lock
	err := s.tx(ctx, true, func(tx *sqlx.Tx, _ time.Time) error {
		var rows []lockRow
		if err := tx.SelectContext(ctx, &rows,
			"SELECT "+lockCols+" FROM locks ORDER BY position, name"); err != nil {
			return err
		}
		for _, r := range rows {
			out = append(out, r.lock())
		}
		return nil
	})
	return out, err
}

// Acquire locks name for user. If user already holds it, the timeout is
// restarted (StatusRefreshed). If someone else holds it, nothing changes
// (StatusHeld).
func (s *Store) Acquire(ctx context.Context, name string, user User) (LockResult, error) {
	return s.lockOp(ctx, name, user, false)
}

// Takeover forcibly takes name for user, resetting ownership and timeout. The
// previous holder is recorded in the audit log. If the lock was free or already
// held by user it behaves like Acquire.
func (s *Store) Takeover(ctx context.Context, name string, user User) (LockResult, error) {
	return s.lockOp(ctx, name, user, true)
}

func (s *Store) lockOp(ctx context.Context, name string, user User, force bool) (LockResult, error) {
	var res LockResult
	err := s.tx(ctx, true, func(tx *sqlx.Tx, now time.Time) error {
		l, err := getLock(ctx, tx, name)
		if err != nil {
			return err
		}

		switch {
		case !l.Locked():
			if err := setHolder(ctx, tx, &l, user, now); err != nil {
				return err
			}
			res = LockResult{Status: StatusAcquired, Lock: l}
			return insertAudit(ctx, tx, AuditEntry{
				LockName: l.Name, Action: ActionLock, UserID: user.ID, UserName: user.Name,
			}, now)

		case l.HolderID == user.ID:
			if err := setHolder(ctx, tx, &l, user, now); err != nil {
				return err
			}
			res = LockResult{Status: StatusRefreshed, Lock: l}
			return insertAudit(ctx, tx, AuditEntry{
				LockName: l.Name, Action: ActionRelock, UserID: user.ID, UserName: user.Name,
			}, now)

		case !force:
			res = LockResult{Status: StatusHeld, Lock: l}
			return nil

		default:
			prev := l
			if err := setHolder(ctx, tx, &l, user, now); err != nil {
				return err
			}
			res = LockResult{Status: StatusTookOver, Lock: l, Previous: prev}
			return insertAudit(ctx, tx, AuditEntry{
				LockName: l.Name, Action: ActionTakeover,
				UserID: user.ID, UserName: user.Name,
				PrevUserID: prev.HolderID, PrevUserName: prev.HolderName,
				HeldFor: prev.HeldFor(now),
			}, now)
		}
	})
	return res, err
}

// Release unlocks name if user is the holder.
func (s *Store) Release(ctx context.Context, name string, user User) (UnlockResult, error) {
	var res UnlockResult
	err := s.tx(ctx, true, func(tx *sqlx.Tx, now time.Time) error {
		l, err := getLock(ctx, tx, name)
		if err != nil {
			return err
		}

		switch {
		case !l.Locked():
			res = UnlockResult{Status: NotLocked, Lock: l}
			return nil
		case l.HolderID != user.ID:
			res = UnlockResult{Status: NotOwner, Lock: l}
			return nil
		}

		if err := clearHolder(ctx, tx, l.Name); err != nil {
			return err
		}
		held := l.HeldFor(now)
		res = UnlockResult{Status: Unlocked, Lock: l, HeldFor: held}
		return insertAudit(ctx, tx, AuditEntry{
			LockName: l.Name, Action: ActionUnlock, UserID: user.ID, UserName: user.Name,
			HeldFor: held,
		}, now)
	})
	return res, err
}

// Audit returns up to limit audit entries for a lock, newest first, along with
// the lock's canonical name. History is available for locks that have since
// been removed from the configuration; ErrNotFound is returned only when the
// name is unknown to both the lock table and the audit log.
func (s *Store) Audit(ctx context.Context, name string, limit int) (string, []AuditEntry, error) {
	var rows []auditRow
	err := s.db.SelectContext(ctx, &rows, `
		SELECT id, lock_name, action, user_id, user_name, prev_user_id, prev_user_name,
		       held_seconds, detail, at
		FROM audit_log WHERE lock_name = ? ORDER BY id DESC LIMIT ?`, name, limit)
	if err != nil {
		return "", nil, err
	}

	entries := make([]AuditEntry, 0, len(rows))
	for _, r := range rows {
		entries = append(entries, r.entry())
	}

	var canonical string
	err = s.db.GetContext(ctx, &canonical, "SELECT name FROM locks WHERE name = ?", name)
	switch {
	case err == nil:
		return canonical, entries, nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", nil, err
	case len(entries) > 0:
		return entries[0].LockName, entries, nil // removed lock with history
	default:
		return "", nil, ErrNotFound
	}
}

// SyncLocks merges the desired lock configuration into the database:
//   - locks already present keep their holder and timestamps; only their
//     timeout (and display order/case) is updated;
//   - locks no longer configured are deleted (their audit history is kept);
//   - new locks are created, unlocked.
//
// It deliberately does not expire locks first: the new timeouts must be
// applied before deciding what has timed out. Call ExpireDue afterwards.
func (s *Store) SyncLocks(ctx context.Context, specs []LockSpec) (SyncSummary, error) {
	var sum SyncSummary
	err := s.tx(ctx, false, func(tx *sqlx.Tx, now time.Time) error {
		var rows []lockRow
		if err := tx.SelectContext(ctx, &rows,
			"SELECT "+lockCols+" FROM locks ORDER BY position, name"); err != nil {
			return err
		}
		existing := make(map[string]lockRow, len(rows))
		for _, r := range rows {
			existing[strings.ToLower(r.Name)] = r
		}

		wanted := make(map[string]bool, len(specs))
		for i, sp := range specs {
			key := strings.ToLower(sp.Name)
			wanted[key] = true
			secs := int64(sp.Timeout / time.Second)

			old, ok := existing[key]
			if !ok {
				if _, err := tx.ExecContext(ctx,
					"INSERT INTO locks (name, position, timeout_seconds) VALUES (?, ?, ?)",
					sp.Name, i, secs); err != nil {
					return err
				}
				if err := insertAudit(ctx, tx, AuditEntry{
					LockName: sp.Name, Action: ActionConfig,
					Detail: "added (timeout " + textfmt.Duration(sp.Timeout) + ")",
				}, now); err != nil {
					return err
				}
				sum.Added = append(sum.Added, sp.Name)
				continue
			}

			if old.TimeoutSeconds != secs || old.Name != sp.Name || old.Position != i {
				if _, err := tx.ExecContext(ctx,
					"UPDATE locks SET name = ?, position = ?, timeout_seconds = ? WHERE name = ?",
					sp.Name, i, secs, old.Name); err != nil {
					return err
				}
			}
			if old.TimeoutSeconds != secs {
				oldTimeout := time.Duration(old.TimeoutSeconds) * time.Second
				if err := insertAudit(ctx, tx, AuditEntry{
					LockName: sp.Name, Action: ActionConfig,
					Detail: "timeout " + textfmt.Duration(oldTimeout) + " → " + textfmt.Duration(sp.Timeout),
				}, now); err != nil {
					return err
				}
				sum.Updated = append(sum.Updated, sp.Name)
			}
		}

		for _, r := range rows {
			if wanted[strings.ToLower(r.Name)] {
				continue
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM locks WHERE name = ?", r.Name); err != nil {
				return err
			}
			detail := "removed from configuration"
			if r.HolderID != "" {
				detail += " (was locked by @" + r.HolderName + ")"
			}
			if err := insertAudit(ctx, tx, AuditEntry{
				LockName: r.Name, Action: ActionConfig, Detail: detail,
			}, now); err != nil {
				return err
			}
			sum.Removed = append(sum.Removed, r.Name)
		}
		return nil
	})
	return sum, err
}
