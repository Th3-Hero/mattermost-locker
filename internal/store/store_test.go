package store

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

var (
	alice = User{ID: "id-alice", Name: "alice"}
	bob   = User{ID: "id-bob", Name: "bob"}
	ctx   = context.Background()
)

func newClock() *clock { return &clock{t: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)} }

func openAt(t *testing.T, path string, c *clock) *Store {
	t.Helper()
	s, err := Open(path, WithClock(c.Now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func setup(t *testing.T, specs ...LockSpec) (*Store, *clock) {
	t.Helper()
	c := newClock()
	s := openAt(t, filepath.Join(t.TempDir(), "test.db"), c)
	if len(specs) == 0 {
		specs = []LockSpec{{"lock1", 24 * time.Hour}, {"lock2", 2 * time.Hour}, {"lock3", 36 * time.Minute}}
	}
	if _, err := s.SyncLocks(ctx, specs); err != nil {
		t.Fatal(err)
	}
	return s, c
}

func mustAcquire(t *testing.T, s *Store, name string, u User) LockResult {
	t.Helper()
	r, err := s.Acquire(ctx, name, u)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func find(t *testing.T, s *Store, name string) Lock {
	t.Helper()
	locks, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range locks {
		if l.Name == name {
			return l
		}
	}
	t.Fatalf("lock %q not found", name)
	return Lock{}
}

func TestSyncCreatesLocksInConfigOrder(t *testing.T) {
	s, _ := setup(t)
	locks, _ := s.List(ctx)
	if len(locks) != 3 || locks[0].Name != "lock1" || locks[2].Name != "lock3" {
		t.Fatalf("unexpected locks: %+v", locks)
	}
	if locks[0].Timeout != 24*time.Hour || locks[0].Locked() {
		t.Errorf("bad initial state: %+v", locks[0])
	}
}

func TestAcquireHeldRefreshAndRelease(t *testing.T) {
	s, c := setup(t)

	r := mustAcquire(t, s, "lock1", alice)
	if r.Status != StatusAcquired || r.Lock.HolderName != "alice" {
		t.Fatalf("acquire: %+v", r)
	}
	if want := c.Now().Add(24 * time.Hour); !r.Lock.ExpiresAt().Equal(want) {
		t.Errorf("expires %v, want %v", r.Lock.ExpiresAt(), want)
	}

	// Someone else cannot take it and learns who has it.
	c.Advance(time.Hour)
	r = mustAcquire(t, s, "LOCK1", bob) // case-insensitive
	if r.Status != StatusHeld || r.Lock.HolderID != alice.ID || r.Lock.Name != "lock1" {
		t.Fatalf("held: %+v", r)
	}

	// Owner re-locking refreshes the timeout.
	r = mustAcquire(t, s, "lock1", alice)
	if r.Status != StatusRefreshed {
		t.Fatalf("refresh: %+v", r)
	}
	if want := c.Now().Add(24 * time.Hour); !r.Lock.ExpiresAt().Equal(want) {
		t.Errorf("timeout not refreshed: %v want %v", r.Lock.ExpiresAt(), want)
	}

	// Non-owner cannot unlock; lock unchanged.
	ur, err := s.Release(ctx, "lock1", bob)
	if err != nil || ur.Status != NotOwner || ur.Lock.HolderID != alice.ID {
		t.Fatalf("non-owner release: %+v %v", ur, err)
	}
	if !find(t, s, "lock1").Locked() {
		t.Fatal("lock should still be held")
	}

	// Owner can; unlocking again says not locked.
	c.Advance(3 * time.Hour)
	ur, _ = s.Release(ctx, "lock1", alice)
	if ur.Status != Unlocked || ur.HeldFor != 3*time.Hour {
		t.Fatalf("release: %+v", ur)
	}
	ur, _ = s.Release(ctx, "lock1", alice)
	if ur.Status != NotLocked {
		t.Fatalf("second release: %+v", ur)
	}
}

func TestOwnershipUsesUserIDNotName(t *testing.T) {
	s, _ := setup(t)
	mustAcquire(t, s, "lock1", alice)

	// Someone else who happens to have alice's old username must not unlock it.
	imposter := User{ID: "id-other", Name: "alice"}
	if r, _ := s.Release(ctx, "lock1", imposter); r.Status != NotOwner {
		t.Fatalf("imposter released lock: %+v", r)
	}
	// The real owner, renamed, still can (and the display name is updated on refresh).
	renamed := User{ID: alice.ID, Name: "alice2"}
	if r := mustAcquire(t, s, "lock1", renamed); r.Status != StatusRefreshed || r.Lock.HolderName != "alice2" {
		t.Fatalf("renamed refresh: %+v", r)
	}
	if r, _ := s.Release(ctx, "lock1", renamed); r.Status != Unlocked {
		t.Fatalf("renamed release: %+v", r)
	}
}

func TestUnknownLock(t *testing.T) {
	s, _ := setup(t)
	if _, err := s.Acquire(ctx, "nope", alice); err != ErrNotFound {
		t.Errorf("acquire: %v", err)
	}
	if _, err := s.Release(ctx, "nope", alice); err != ErrNotFound {
		t.Errorf("release: %v", err)
	}
	if _, err := s.Takeover(ctx, "nope", alice); err != ErrNotFound {
		t.Errorf("takeover: %v", err)
	}
	if _, _, err := s.Audit(ctx, "nope", 10); err != ErrNotFound {
		t.Errorf("audit: %v", err)
	}
}

func TestTakeover(t *testing.T) {
	s, c := setup(t)
	mustAcquire(t, s, "lock1", alice)
	c.Advance(5 * time.Hour)

	r, err := s.Takeover(ctx, "lock1", bob)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusTookOver || r.Lock.HolderID != bob.ID || r.Previous.HolderID != alice.ID {
		t.Fatalf("takeover: %+v", r)
	}
	// Ownership and timeout are reset.
	if want := c.Now().Add(24 * time.Hour); !r.Lock.ExpiresAt().Equal(want) {
		t.Errorf("expiry %v want %v", r.Lock.ExpiresAt(), want)
	}
	// Alice has lost it.
	if ur, _ := s.Release(ctx, "lock1", alice); ur.Status != NotOwner {
		t.Errorf("alice should no longer own the lock: %+v", ur)
	}

	// Audited with who/from-whom/how long.
	_, entries, _ := s.Audit(ctx, "lock1", 10)
	got := entries[0]
	if got.Action != ActionTakeover || got.UserName != "bob" || got.PrevUserName != "alice" ||
		got.PrevUserID != alice.ID || got.HeldFor != 5*time.Hour {
		t.Errorf("takeover audit: %+v", got)
	}
}

func TestTakeoverOfFreeOrOwnLockBehavesLikeLock(t *testing.T) {
	s, _ := setup(t)
	if r, _ := s.Takeover(ctx, "lock1", alice); r.Status != StatusAcquired {
		t.Errorf("free: %+v", r)
	}
	if r, _ := s.Takeover(ctx, "lock1", alice); r.Status != StatusRefreshed {
		t.Errorf("own: %+v", r)
	}
}

func TestTimeoutReleasesAndNotifiesOnce(t *testing.T) {
	s, c := setup(t)
	var got []Expiry
	s.SetExpiryHook(func(e []Expiry) { got = append(got, e...) })

	mustAcquire(t, s, "lock3", alice) // 36m
	c.Advance(35 * time.Minute)
	if err := s.ExpireDue(ctx); err != nil || len(got) != 0 {
		t.Fatalf("expired too early: %v %v", err, got)
	}

	c.Advance(time.Minute) // exactly at timeout
	if err := s.ExpireDue(ctx); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Lock.Name != "lock3" || got[0].Lock.HolderName != "alice" {
		t.Fatalf("expiry events: %+v", got)
	}
	if find(t, s, "lock3").Locked() {
		t.Error("lock3 should be free")
	}

	// Only once.
	c.Advance(time.Hour)
	_ = s.ExpireDue(ctx)
	if len(got) != 1 {
		t.Errorf("duplicate expiry events: %d", len(got))
	}

	// Audited, timestamped at the actual expiry moment.
	_, entries, _ := s.Audit(ctx, "lock3", 10)
	if entries[0].Action != ActionTimeout || entries[0].UserName != "alice" ||
		!entries[0].At.Equal(time.Date(2026, 11, 11, 12, 36, 0, 0, time.UTC)) {
		t.Errorf("timeout audit: %+v", entries[0])
	}
}

func TestOperationsExpireStaleLocksFirst(t *testing.T) {
	// Even if the background loop hasn't run, a lock past its timeout is
	// released (and announced) before the next operation proceeds.
	s, c := setup(t)
	var got []Expiry
	s.SetExpiryHook(func(e []Expiry) { got = append(got, e...) })

	mustAcquire(t, s, "lock3", alice)
	c.Advance(time.Hour)

	r := mustAcquire(t, s, "lock3", bob)
	if r.Status != StatusAcquired || r.Lock.HolderID != bob.ID {
		t.Fatalf("bob should get the expired lock: %+v", r)
	}
	if len(got) != 1 || got[0].Lock.HolderName != "alice" {
		t.Fatalf("alice's timeout should have been announced: %+v", got)
	}
}

func TestRestartDoesNotDisturbLocks(t *testing.T) {
	c := newClock()
	path := filepath.Join(t.TempDir(), "restart.db")
	specs := []LockSpec{{"lock1", 24 * time.Hour}, {"lock2", 2 * time.Hour}}

	s1 := openAt(t, path, c)
	s1.SyncLocks(ctx, specs)
	mustAcquire(t, s1, "lock1", alice)
	mustAcquire(t, s1, "lock2", bob)
	s1.Close()

	// Offline for 3h: lock2 (2h) timed out meanwhile, lock1 (24h) did not.
	c.Advance(3 * time.Hour)

	s2 := openAt(t, path, c)
	var got []Expiry
	s2.SetExpiryHook(func(e []Expiry) { got = append(got, e...) })
	if _, err := s2.SyncLocks(ctx, specs); err != nil {
		t.Fatal(err)
	}
	// Sync alone must not release or announce anything.
	if len(got) != 0 {
		t.Fatalf("SyncLocks must not expire locks: %+v", got)
	}
	_ = s2.ExpireDue(ctx)

	l1 := find(t, s2, "lock1")
	if !l1.Locked() || l1.HolderID != alice.ID {
		t.Errorf("lock1 must survive restart: %+v", l1)
	}
	if find(t, s2, "lock2").Locked() {
		t.Error("lock2 timed out while offline and should be released")
	}
	if len(got) != 1 || got[0].Lock.Name != "lock2" {
		t.Errorf("expected exactly lock2's timeout to be announced: %+v", got)
	}
}

func TestSyncDoesNotExpireBeforeApplyingNewTimeouts(t *testing.T) {
	// A lock that would have expired under its OLD timeout survives if the
	// configuration lengthened the timeout while the app was offline.
	c := newClock()
	path := filepath.Join(t.TempDir(), "len.db")

	s1 := openAt(t, path, c)
	s1.SyncLocks(ctx, []LockSpec{{"lock1", time.Hour}})
	mustAcquire(t, s1, "lock1", alice)
	s1.Close()

	c.Advance(2 * time.Hour) // past the old 1h timeout

	s2 := openAt(t, path, c)
	s2.SyncLocks(ctx, []LockSpec{{"lock1", 24 * time.Hour}})
	if err := s2.ExpireDue(ctx); err != nil {
		t.Fatal(err)
	}
	l := find(t, s2, "lock1")
	if !l.Locked() || l.HolderID != alice.ID {
		t.Fatalf("lock should still be held under the new 24h timeout: %+v", l)
	}
	if want := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC); !l.ExpiresAt().Equal(want) {
		t.Errorf("expiry %v want %v", l.ExpiresAt(), want)
	}
}

func TestShortenedTimeoutReleasesLock(t *testing.T) {
	c := newClock()
	path := filepath.Join(t.TempDir(), "short.db")

	s1 := openAt(t, path, c)
	s1.SyncLocks(ctx, []LockSpec{{"lock1", 24 * time.Hour}})
	mustAcquire(t, s1, "lock1", alice)
	s1.Close()

	c.Advance(3 * time.Hour)

	s2 := openAt(t, path, c)
	var got []Expiry
	s2.SetExpiryHook(func(e []Expiry) { got = append(got, e...) })
	s2.SyncLocks(ctx, []LockSpec{{"lock1", 2 * time.Hour}}) // now 3h held > 2h
	_ = s2.ExpireDue(ctx)

	if find(t, s2, "lock1").Locked() {
		t.Error("lock held longer than the new timeout should be released")
	}
	if len(got) != 1 {
		t.Errorf("expected announcement: %+v", got)
	}
	// Expiry is recorded at locked_at + new timeout.
	if want := time.Date(2026, 11, 11, 14, 0, 0, 0, time.UTC); !got[0].ExpiredAt.Equal(want) {
		t.Errorf("ExpiredAt %v want %v", got[0].ExpiredAt, want)
	}
}

func TestSyncMergesConfiguration(t *testing.T) {
	s, _ := setup(t)
	mustAcquire(t, s, "lock1", alice)
	mustAcquire(t, s, "lock2", bob)

	sum, err := s.SyncLocks(ctx, []LockSpec{
		{"lock1", 48 * time.Hour}, // timeout changed, state kept
		{"lock4", time.Hour},      // new
		// lock2 removed (while held), lock3 removed
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Added) != 1 || sum.Added[0] != "lock4" ||
		len(sum.Updated) != 1 || sum.Updated[0] != "lock1" ||
		len(sum.Removed) != 2 {
		t.Fatalf("summary: %+v", sum)
	}

	locks, _ := s.List(ctx)
	if len(locks) != 2 || locks[0].Name != "lock1" || locks[1].Name != "lock4" {
		t.Fatalf("locks: %+v", locks)
	}
	if l := locks[0]; l.Timeout != 48*time.Hour || l.HolderID != alice.ID {
		t.Errorf("lock1 should keep holder, new timeout: %+v", l)
	}
	if locks[1].Locked() {
		t.Error("new lock should be free")
	}

	// History of removed locks survives and explains the removal.
	name, entries, err := s.Audit(ctx, "lock2", 10)
	if err != nil || name != "lock2" {
		t.Fatalf("audit of removed lock: %q %v", name, err)
	}
	if entries[0].Action != ActionConfig || entries[0].Detail != "removed from configuration (was locked by @bob)" {
		t.Errorf("removal audit: %+v", entries[0])
	}
	if _, err := s.Acquire(ctx, "lock2", alice); err != ErrNotFound {
		t.Errorf("removed lock must not be lockable: %v", err)
	}
}

func TestSyncIsIdempotent(t *testing.T) {
	s, _ := setup(t)
	mustAcquire(t, s, "lock1", alice)
	specs := []LockSpec{{"lock1", 24 * time.Hour}, {"lock2", 2 * time.Hour}, {"lock3", 36 * time.Minute}}
	sum, err := s.SyncLocks(ctx, specs)
	if err != nil || len(sum.Added)+len(sum.Updated)+len(sum.Removed) != 0 {
		t.Fatalf("no-op sync changed things: %+v %v", sum, err)
	}
	if !find(t, s, "lock1").Locked() {
		t.Error("lock state lost")
	}
}

func TestSyncCaseChangeKeepsLock(t *testing.T) {
	s, _ := setup(t, LockSpec{"build", time.Hour})
	mustAcquire(t, s, "build", alice)
	if _, err := s.SyncLocks(ctx, []LockSpec{{"Build", time.Hour}}); err != nil {
		t.Fatal(err)
	}
	l := find(t, s, "Build")
	if !l.Locked() || l.HolderID != alice.ID {
		t.Errorf("renaming case must keep the lock: %+v", l)
	}
}

func TestAuditOrderLimitAndHistory(t *testing.T) {
	s, c := setup(t)
	mustAcquire(t, s, "lock1", alice) // lock
	c.Advance(time.Minute)
	mustAcquire(t, s, "lock1", alice) // relock
	c.Advance(time.Minute)
	s.Release(ctx, "lock1", alice) // unlock
	c.Advance(time.Minute)
	mustAcquire(t, s, "lock1", bob) // lock

	name, entries, err := s.Audit(ctx, "LOCK1", 3)
	if err != nil || name != "lock1" || len(entries) != 3 {
		t.Fatalf("audit: %q %d %v", name, len(entries), err)
	}
	want := []Action{ActionLock, ActionUnlock, ActionRelock} // newest first, limited to 3
	for i, a := range want {
		if entries[i].Action != a {
			t.Errorf("entry %d = %s, want %s", i, entries[i].Action, a)
		}
	}

	// Failed attempts and no-ops are not audited.
	mustAcquire(t, s, "lock1", alice) // held by bob
	s.Release(ctx, "lock1", alice)    // not owner
	_, all, _ := s.Audit(ctx, "lock1", 50)
	if len(all) != 5 { // config added + 4 events
		t.Errorf("expected 5 entries (config + 4), got %d", len(all))
	}
}

func TestAuditEmptyForKnownLock(t *testing.T) {
	s, _ := setup(t)
	// Make sure config "added" rows don't count as missing: lock3 has just that one.
	name, entries, err := s.Audit(ctx, "lock3", 10)
	if err != nil || name != "lock3" || len(entries) != 1 || entries[0].Action != ActionConfig {
		t.Fatalf("%q %+v %v", name, entries, err)
	}
}

func TestConcurrentAcquireHasExactlyOneWinner(t *testing.T) {
	s, _ := setup(t)

	const n = 25
	var wg sync.WaitGroup
	var winners atomic.Int32
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			u := User{ID: "id-" + string(rune('a'+i)), Name: "user"}
			r, err := s.Acquire(ctx, "lock1", u)
			if err != nil {
				t.Error(err)
				return
			}
			if r.Status == StatusAcquired {
				winners.Add(1)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if winners.Load() != 1 {
		t.Fatalf("expected exactly one winner, got %d", winners.Load())
	}
}

func TestRunExpiryLoop(t *testing.T) {
	// Real clock: a 1s lock should be released and announced by the loop.
	s, err := Open(filepath.Join(t.TempDir(), "loop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SyncLocks(ctx, []LockSpec{{"quick", time.Second}})

	events := make(chan Expiry, 1)
	s.SetExpiryHook(func(e []Expiry) { events <- e[0] })
	if _, err := s.Acquire(ctx, "quick", alice); err != nil {
		t.Fatal(err)
	}

	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go s.RunExpiryLoop(loopCtx, 100*time.Millisecond)

	select {
	case e := <-events:
		if e.Lock.Name != "quick" {
			t.Errorf("unexpected: %+v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("expiry loop never released the lock")
	}
}
