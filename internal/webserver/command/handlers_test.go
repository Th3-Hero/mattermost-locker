package command

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mattermost-locker/internal/announce"
	"mattermost-locker/internal/store"
	"mattermost-locker/internal/textfmt"
)

type recorder struct {
	mu   sync.Mutex
	msgs []string
}

func (r *recorder) Notify(text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, text)
}

func (r *recorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.msgs
	r.msgs = nil
	return out
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

type env struct {
	h     *Handlers
	st    *store.Store
	clock *fakeClock
	chat  *recorder // what gets posted to the locks channel
}

func setup(t *testing.T) *env {
	t.Helper()
	clock := &fakeClock{t: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)}
	st, err := store.Open(filepath.Join(t.TempDir(), "cmd.db"), store.WithClock(clock.Now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.SyncLocks(context.Background(), []store.LockSpec{
		{Name: "lock1", Timeout: 24 * time.Hour},
		{Name: "lock2", Timeout: 2 * time.Hour},
		{Name: "lock3", Timeout: 36 * time.Minute},
	}); err != nil {
		t.Fatal(err)
	}

	chat := &recorder{}
	f := textfmt.Formatter{Loc: time.UTC, Now: clock.Now}
	ann := announce.New(chat, f)
	st.SetExpiryHook(ann.TimedOut)
	h := New(st, ann, f, Options{AuditDefaultLimit: 10, AuditMaxLimit: 100})
	return &env{h: h, st: st, clock: clock, chat: chat}
}

func (e *env) run(t *testing.T, user, text string) *Response {
	t.Helper()
	resp, err := e.h.Dispatch(context.Background(), &Request{
		Command: "/lock-bot", Text: text, UserID: "id-" + user, UserName: user,
	})
	if err != nil {
		t.Fatalf("%q: %v", text, err)
	}
	if resp.ResponseType != "ephemeral" {
		t.Fatalf("%q: replies must be ephemeral, got %q", text, resp.ResponseType)
	}
	return resp
}

func mustContain(t *testing.T, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Errorf("missing %q in:\n%s", sub, s)
		}
	}
}

func TestLockFlowAndChannelAnnouncements(t *testing.T) {
	e := setup(t)

	r := e.run(t, "alice", "lock lock1")
	mustContain(t, r.Text, "You locked **lock1**", "2026-11-12 12:00 UTC (in 1 day)")
	msgs := e.chat.take()
	if len(msgs) != 1 {
		t.Fatalf("expected one channel post, got %v", msgs)
	}
	mustContain(t, msgs[0], "🔒", "**lock1**", "locked by alice")

	// Someone else tries: ephemeral failure with who/when, and NOTHING posted.
	e.clock.Advance(4 * time.Hour)
	r = e.run(t, "bob", "lock lock1")
	mustContain(t, r.Text, "already locked by @alice", "2026-11-11 12:00 UTC (4 hours ago)")
	if msgs := e.chat.take(); len(msgs) != 0 {
		t.Errorf("failed lock must not be posted publicly: %v", msgs)
	}

	// Owner re-locks: told they own it, nothing posted.
	r = e.run(t, "alice", "lock lock1")
	mustContain(t, r.Text, "already own **lock1**", "refreshed")
	if msgs := e.chat.take(); len(msgs) != 0 {
		t.Errorf("refresh should not be posted: %v", msgs)
	}

	// Owner unlocks: posted.
	e.clock.Advance(time.Hour)
	r = e.run(t, "alice", "unlock lock1")
	mustContain(t, r.Text, "You unlocked **lock1**")
	msgs = e.chat.take()
	if len(msgs) != 1 {
		t.Fatalf("expected unlock post, got %v", msgs)
	}
	mustContain(t, msgs[0], "🔓", "unlocked by alice", "held for 1 hour")

	r = e.run(t, "alice", "unlock lock1")
	mustContain(t, r.Text, "not locked")
}

func TestUnlockByNonOwnerSuggestsTakeoverWithWarning(t *testing.T) {
	e := setup(t)
	e.run(t, "alice", "lock lock2")
	e.chat.take()

	r := e.run(t, "bob", "unlock lock2")
	mustContain(t, r.Text,
		"locked by @alice", "can't unlock",
		"confident the lock was forgotten", "not intentionally left",
		"`/lock-bot takeover lock2`", "check with @alice")
	if msgs := e.chat.take(); len(msgs) != 0 {
		t.Errorf("nothing should be posted: %v", msgs)
	}
}

func TestTakeover(t *testing.T) {
	e := setup(t)
	e.run(t, "alice", "lock lock2") // 2h timeout
	e.chat.take()
	e.clock.Advance(time.Hour)

	r := e.run(t, "bob", "takeover lock2")
	mustContain(t, r.Text, "took over **lock2**", "@alice", "announced and audited")
	msgs := e.chat.take()
	if len(msgs) != 1 {
		t.Fatalf("expected takeover post, got %v", msgs)
	}
	mustContain(t, msgs[0], "⚠️", "**lock2**", "taken over by bob", "@alice")

	// Alice can no longer unlock; Bob can.
	mustContain(t, e.run(t, "alice", "unlock lock2").Text, "locked by @bob")
	mustContain(t, e.run(t, "bob", "unlock lock2").Text, "You unlocked")

	// Taking over a free lock is just a lock.
	e.chat.take()
	mustContain(t, e.run(t, "bob", "takeover lock3").Text, "You locked **lock3**")
	if msgs := e.chat.take(); len(msgs) != 1 || !strings.Contains(msgs[0], "locked by bob") {
		t.Errorf("free takeover should announce as a lock: %v", msgs)
	}

	r = e.run(t, "carol", "audit lock2")
	mustContain(t, r.Text, "takeover", "from @alice", "held for 1 hour")
}

func TestTimeoutIsAnnouncedToChannel(t *testing.T) {
	e := setup(t)
	e.run(t, "alice", "lock lock3") // 36m
	e.chat.take()

	e.clock.Advance(37 * time.Minute)
	if err := e.st.ExpireDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	msgs := e.chat.take()
	if len(msgs) != 1 {
		t.Fatalf("expected timeout post, got %v", msgs)
	}
	mustContain(t, msgs[0], "⏰", "**lock3**", "timed out", "@alice")

	mustContain(t, e.run(t, "bob", "lock lock3").Text, "You locked **lock3**")
}

func TestStatus(t *testing.T) {
	e := setup(t)
	e.run(t, "alice", "lock lock1")
	e.clock.Advance(4 * time.Hour)
	e.run(t, "bob", "lock lock3")

	r := e.run(t, "carol", "status")
	mustContain(t, r.Text,
		"| lock1 | 🔒 Locked | @alice | 2026-11-11 12:00 UTC (4 hours ago) | 2026-11-12 12:00 UTC (in 20 hours) | 24h |",
		"| lock2 | 🔓 Free | | | | 2h |",
		"| lock3 | 🔒 Locked | @bob |")
	if strings.Index(r.Text, "lock1") > strings.Index(r.Text, "lock2") ||
		strings.Index(r.Text, "lock2") > strings.Index(r.Text, "lock3") {
		t.Error("locks should be listed in configuration order")
	}
}

func TestAuditLimitsAndFlags(t *testing.T) {
	e := setup(t)
	for i := 0; i < 8; i++ {
		e.run(t, "alice", "lock lock1")
		e.clock.Advance(time.Minute)
		e.run(t, "alice", "unlock lock1")
		e.clock.Advance(time.Minute)
	}
	rows := func(text string) int { return strings.Count(text, "\n| 20") }

	if n := rows(e.run(t, "bob", "audit lock1").Text); n != 10 {
		t.Errorf("default limit: got %d rows, want 10", n)
	}
	for _, args := range []string{"audit lock1 -n 3", "audit lock1 -n=3", "audit lock1 -n3", "audit -n 3 lock1"} {
		if n := rows(e.run(t, "bob", args).Text); n != 3 {
			t.Errorf("%q: got %d rows, want 3", args, n)
		}
	}
	r := e.run(t, "bob", "audit lock1 -n 500")
	mustContain(t, r.Text, "limited to the maximum of 100")
	if n := rows(r.Text); n != 17 { // 8*2 + config "added"
		t.Errorf("max: got %d rows, want 17", n)
	}

	// Newest first.
	r = e.run(t, "bob", "audit lock1 -n 2")
	if strings.Index(r.Text, "unlock") > strings.Index(r.Text, "| lock |") && strings.Contains(r.Text, "| lock |") {
		t.Errorf("not newest-first:\n%s", r.Text)
	}
	mustContain(t, r.Text, "**Audit log for lock1**", "| When | Action | User | Details |", "@alice")
}

func TestAuditAcceptsRemovedLocksAndShowsConfigEvents(t *testing.T) {
	e := setup(t)
	if _, err := e.st.SyncLocks(context.Background(), []store.LockSpec{{Name: "lock1", Timeout: 12 * time.Hour}}); err != nil {
		t.Fatal(err)
	}
	r := e.run(t, "bob", "audit lock2")
	mustContain(t, r.Text, "removed from configuration", "system")
	r = e.run(t, "bob", "audit lock1")
	mustContain(t, r.Text, "timeout 24h → 12h")
}

func TestErrorsAreEphemeralAndHelpful(t *testing.T) {
	e := setup(t)

	mustContain(t, e.run(t, "a", "lock nope").Text, "Unknown lock `nope`", "lock1, lock2, lock3")
	mustContain(t, e.run(t, "a", "unlock nope").Text, "Unknown lock")
	mustContain(t, e.run(t, "a", "takeover nope").Text, "Unknown lock")
	mustContain(t, e.run(t, "a", "audit nope").Text, "Unknown lock")
	mustContain(t, e.run(t, "a", "lock").Text, "Usage: `/lock-bot lock <lock>`")
	mustContain(t, e.run(t, "a", "lock a b").Text, "Usage")
	mustContain(t, e.run(t, "a", "frobnicate").Text, "Unknown command `frobnicate`", "/lock-bot lock")
	mustContain(t, e.run(t, "a", "audit").Text, "give a lock name", "Usage")
	mustContain(t, e.run(t, "a", "audit lock1 -n 0").Text, "positive whole number")
	mustContain(t, e.run(t, "a", "audit lock1 -n abc").Text, "positive whole number")
	mustContain(t, e.run(t, "a", "audit lock1 -n").Text, "needs a number")
	mustContain(t, e.run(t, "a", "audit lock1 --bogus").Text, "Unknown option")
	mustContain(t, e.run(t, "a", "audit lock1 lock2").Text, "Only one lock name")
	mustContain(t, e.run(t, "a", "status extra").Text, "no arguments")

	if msgs := e.chat.take(); len(msgs) != 0 {
		t.Errorf("errors must never be posted publicly: %v", msgs)
	}
}

func TestHelpAndEmptyText(t *testing.T) {
	e := setup(t)
	for _, text := range []string{"", "   ", "help"} {
		mustContain(t, e.run(t, "a", text).Text, "/lock-bot lock <lock>", "/lock-bot takeover <lock>",
			"/lock-bot status", "/lock-bot audit <lock> [-n N]")
	}
}

func TestLockNamesAreCaseInsensitiveAndSubcommandsToo(t *testing.T) {
	e := setup(t)
	mustContain(t, e.run(t, "alice", "LOCK LOCK1").Text, "You locked **lock1**")
}

func TestMissingUserIDIsAnError(t *testing.T) {
	e := setup(t)
	_, err := e.h.Dispatch(context.Background(), &Request{Text: "lock lock1"})
	if err == nil {
		t.Fatal("expected error for request without user_id")
	}
}

func TestUserInputCannotBreakMarkdown(t *testing.T) {
	e := setup(t)
	r := e.run(t, "a", "lock `rm -rf`")
	if strings.Contains(r.Text, "``") {
		t.Errorf("backticks leaked: %s", r.Text)
	}
}
