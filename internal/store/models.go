package store

import (
	"errors"
	"time"
)

// ErrNotFound is returned when a named lock does not exist.
var ErrNotFound = errors.New("lock not found")

// User identifies a Mattermost user. The ID is used for all ownership logic;
// the name is only for display.
type User struct {
	ID   string
	Name string
}

// Lock is the current state of one lockable resource.
type Lock struct {
	Name       string
	Position   int
	Timeout    time.Duration
	HolderID   string
	HolderName string
	LockedAt   time.Time
}

// Locked reports whether somebody currently holds the lock.
func (l Lock) Locked() bool { return l.HolderID != "" }

// ExpiresAt is when the lock times out, based on the *current* timeout setting.
func (l Lock) ExpiresAt() time.Time { return l.LockedAt.Add(l.Timeout) }

// HeldFor is how long the lock has been held as of now.
func (l Lock) HeldFor(now time.Time) time.Duration {
	if !l.Locked() {
		return 0
	}
	return now.Sub(l.LockedAt)
}

// Action is the kind of event recorded in the audit log.
type Action string

const (
	ActionLock     Action = "lock"
	ActionRelock   Action = "relock" // holder re-locked, refreshing the timeout
	ActionUnlock   Action = "unlock"
	ActionTakeover Action = "takeover"
	ActionTimeout  Action = "timeout"
	ActionConfig   Action = "config" // lock added/removed/changed via configuration
)

// AuditEntry is one row of the audit log.
type AuditEntry struct {
	ID           int64
	LockName     string
	Action       Action
	UserID       string // actor (for timeouts: the former holder; empty for config changes)
	UserName     string
	PrevUserID   string // takeovers: the previous holder
	PrevUserName string
	HeldFor      time.Duration // unlock/takeover/timeout: how long it had been held
	Detail       string        // free text, used by config events
	At           time.Time
}

// Expiry describes a lock that timed out and was released.
type Expiry struct {
	Lock      Lock      // state just before release (holder, locked time, ...)
	ExpiredAt time.Time // LockedAt + Timeout
}

// LockStatus is the outcome of Acquire/Takeover.
type LockStatus int

const (
	StatusAcquired  LockStatus = iota // was free, now held by the caller
	StatusRefreshed                   // caller already held it; timeout restarted
	StatusHeld                        // held by someone else (Acquire only); nothing changed
	StatusTookOver                    // forcibly taken from another user (Takeover only)
)

// LockResult is returned by Acquire and Takeover.
type LockResult struct {
	Status LockStatus
	// Lock is the lock after the operation (for StatusHeld: the current holder's lock).
	Lock Lock
	// Previous is the lock before a takeover.
	Previous Lock
}

// UnlockStatus is the outcome of Release.
type UnlockStatus int

const (
	Unlocked  UnlockStatus = iota // released by its owner
	NotLocked                     // was not locked
	NotOwner                      // held by someone else; nothing changed
)

// UnlockResult is returned by Release.
type UnlockResult struct {
	Status UnlockStatus
	// Lock is the lock as it was before the call (holder, locked time, ...).
	Lock Lock
	// HeldFor is how long it had been held (Unlocked only).
	HeldFor time.Duration
}

// LockSpec is the desired configuration of a lock, used by SyncLocks.
type LockSpec struct {
	Name    string
	Timeout time.Duration
}

// SyncSummary reports what SyncLocks changed.
type SyncSummary struct {
	Added   []string
	Updated []string
	Removed []string
}

// ---- database rows ----

type lockRow struct {
	Name           string `db:"name"`
	Position       int    `db:"position"`
	TimeoutSeconds int64  `db:"timeout_seconds"`
	HolderID       string `db:"holder_id"`
	HolderName     string `db:"holder_name"`
	LockedAt       int64  `db:"locked_at"`
}

func (r lockRow) lock() Lock {
	l := Lock{
		Name:       r.Name,
		Position:   r.Position,
		Timeout:    time.Duration(r.TimeoutSeconds) * time.Second,
		HolderID:   r.HolderID,
		HolderName: r.HolderName,
	}
	if r.HolderID != "" {
		l.LockedAt = time.Unix(r.LockedAt, 0)
	}
	return l
}

type auditRow struct {
	ID           int64  `db:"id"`
	LockName     string `db:"lock_name"`
	Action       string `db:"action"`
	UserID       string `db:"user_id"`
	UserName     string `db:"user_name"`
	PrevUserID   string `db:"prev_user_id"`
	PrevUserName string `db:"prev_user_name"`
	HeldSeconds  int64  `db:"held_seconds"`
	Detail       string `db:"detail"`
	At           int64  `db:"at"`
}

func (r auditRow) entry() AuditEntry {
	return AuditEntry{
		ID:           r.ID,
		LockName:     r.LockName,
		Action:       Action(r.Action),
		UserID:       r.UserID,
		UserName:     r.UserName,
		PrevUserID:   r.PrevUserID,
		PrevUserName: r.PrevUserName,
		HeldFor:      time.Duration(r.HeldSeconds) * time.Second,
		Detail:       r.Detail,
		At:           time.Unix(r.At, 0),
	}
}
