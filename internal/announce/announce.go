// Package announce composes the public messages posted to the locks channel.
package announce

import (
	"fmt"

	"mattermost-locker/internal/notify"
	"mattermost-locker/internal/store"
	"mattermost-locker/internal/textfmt"
)

// Announcer turns lock events into channel messages.
//
// People are written as plain usernames unless the message is something they
// need to act on (a takeover of their lock, or their lock timing out), in which
// case they are @mentioned so Mattermost notifies them.
type Announcer struct {
	n notify.Notifier
	f textfmt.Formatter
}

// New creates an Announcer.
func New(n notify.Notifier, f textfmt.Formatter) *Announcer { return &Announcer{n: n, f: f} }

// Locked announces that a lock was acquired (l is the lock after locking).
func (a *Announcer) Locked(l store.Lock) {
	a.n.Notify(fmt.Sprintf("🔒 **%s** was locked by %s. It will time out %s.",
		l.Name, l.HolderName, a.f.When(l.ExpiresAt())))
}

// Unlocked announces a voluntary unlock.
func (a *Announcer) Unlocked(res store.UnlockResult, by store.User) {
	a.n.Notify(fmt.Sprintf("🔓 **%s** was unlocked by %s (held for %s).",
		res.Lock.Name, by.Name, textfmt.Span(res.HeldFor)))
}

// TakenOver announces a forced takeover.
func (a *Announcer) TakenOver(res store.LockResult) {
	a.n.Notify(fmt.Sprintf(
		"⚠️ **%s** was taken over by %s from @%s, who had locked it %s. The lock will now time out %s.",
		res.Lock.Name, res.Lock.HolderName, res.Previous.HolderName,
		a.f.When(res.Previous.LockedAt), a.f.When(res.Lock.ExpiresAt())))
}

// TimedOut announces locks that were automatically released.
func (a *Announcer) TimedOut(expired []store.Expiry) {
	for _, e := range expired {
		a.n.Notify(fmt.Sprintf(
			"⏰ **%s** timed out and was automatically unlocked. It was held by @%s since %s.",
			e.Lock.Name, e.Lock.HolderName, a.f.When(e.Lock.LockedAt)))
	}
}
