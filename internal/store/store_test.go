// Tests for the gateway database.
//
// These lean on the guarantees that are invisible in the API surface and
// therefore easy to break by "tidying" a query: that a credential change
// invalidates sessions minted before it, that a disabled account cannot
// authenticate and has its sessions swept, that provisioning an app twice is
// not an error, and that the throttle actually escalates. Everything runs
// against a real SQLite file in t.TempDir() — an in-memory stand-in would not
// exercise the schema, the ON CONFLICT clauses or the driver's error strings,
// which is where the interesting failures live.
//
// Nothing here sleeps. Lockouts are asserted on the durations the store
// returns and stores, not by waiting them out.
package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// WAL leaves -wal/-shm siblings; closing before TempDir cleanup keeps
	// Windows from failing the removal on open handles.
	t.Cleanup(func() { s.Close() })
	return s
}

func mustUser(t *testing.T, s *Store, name string) *User {
	t.Helper()
	u, err := s.CreateUser(context.Background(), name, "pw-hash-"+name, "phrase-hash-"+name, false)
	if err != nil {
		t.Fatalf("CreateUser(%q): %v", name, err)
	}
	return u
}

func TestCreateAndLookupUser(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	u, err := s.CreateUser(ctx, "ada", "pw-hash", "phrase-hash", true)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if u.ID == 0 {
		t.Fatal("CreateUser returned a zero ID")
	}
	if !u.IsAdmin {
		t.Error("IsAdmin was not persisted")
	}
	if u.IsDisabled {
		t.Error("a new user is disabled")
	}
	// Sessions are keyed to this value; starting anywhere but 1 would mean
	// CreateSession recorded a generation the user row never had.
	if u.CredentialGen != 1 {
		t.Errorf("CredentialGen = %d, want 1", u.CredentialGen)
	}
	if u.CreatedAt.IsZero() {
		t.Error("CreatedAt did not round trip through the RFC3339 text column")
	}
	if u.LastLoginAt != nil {
		t.Error("a user who has never logged in has a LastLoginAt")
	}

	byName, err := s.UserByName(ctx, "ada")
	if err != nil {
		t.Fatalf("UserByName: %v", err)
	}
	byID, err := s.UserByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if byName.ID != u.ID || byID.Username != "ada" {
		t.Fatalf("lookups disagree: byName=%+v byID=%+v", byName, byID)
	}
	if byName.PasswordHash != "pw-hash" || byName.PassphraseHash != "phrase-hash" {
		t.Errorf("hashes did not round trip: %q / %q", byName.PasswordHash, byName.PassphraseHash)
	}

	// The signup path distinguishes "taken" from every other failure so it can
	// say so; a plain error would surface as a 500.
	if _, err := s.CreateUser(ctx, "ada", "other-hash", "other-phrase", false); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("duplicate CreateUser = %v, want ErrUsernameTaken", err)
	}
	if n, _ := s.CountUsers(ctx); n != 1 {
		t.Fatalf("CountUsers = %d after a rejected duplicate, want 1", n)
	}

	if _, err := s.UserByName(ctx, "grace"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UserByName(unknown) = %v, want ErrNotFound", err)
	}
	if _, err := s.UserByID(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UserByID(unknown) = %v, want ErrNotFound", err)
	}
}

func TestSessionRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u := mustUser(t, s, "ada")

	const token = "opaque-cookie-value"
	if err := s.CreateSession(ctx, token, u, time.Hour, "Firefox", "203.0.113.7"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, err := s.SessionUser(ctx, token)
	if err != nil {
		t.Fatalf("SessionUser: %v", err)
	}
	if got.ID != u.ID || got.Username != "ada" {
		t.Fatalf("SessionUser returned %+v, want user %d", got, u.ID)
	}

	if _, err := s.SessionUser(ctx, "some-other-token"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SessionUser(unknown) = %v, want ErrNotFound", err)
	}

	// The stored key is a hash of the token, so the raw token must not be
	// sitting in the table under its own name.
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM sessions WHERE token_hash = ?`, token).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatal("the plaintext token is stored as the primary key; a database leak would be replayable")
	}

	if err := s.DeleteSession(ctx, token); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := s.SessionUser(ctx, token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SessionUser after DeleteSession = %v, want ErrNotFound", err)
	}
}

// This is the "changing your password logs out every other device" promise. It
// is enforced by a join on credential_gen rather than by deleting rows, so it
// is exactly the sort of thing a well-meaning query rewrite would drop.
func TestSetCredentialsInvalidatesExistingSessions(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u := mustUser(t, s, "ada")

	const phone, laptop = "phone-token", "laptop-token"
	if err := s.CreateSession(ctx, phone, u, time.Hour, "phone", ""); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := s.CreateSession(ctx, laptop, u, time.Hour, "laptop", ""); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := s.SessionUser(ctx, phone); err != nil {
		t.Fatalf("session should be valid before the reset: %v", err)
	}

	if err := s.SetCredentials(ctx, u.ID, "new-pw-hash", ""); err != nil {
		t.Fatalf("SetCredentials: %v", err)
	}

	after, err := s.UserByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if after.CredentialGen != u.CredentialGen+1 {
		t.Fatalf("CredentialGen = %d, want %d", after.CredentialGen, u.CredentialGen+1)
	}
	if after.PasswordHash != "new-pw-hash" {
		t.Errorf("PasswordHash = %q, want the new hash", after.PasswordHash)
	}
	// An empty passphraseHash means "leave it alone" — the password-change
	// form does not ask for a new recovery phrase.
	if after.PassphraseHash != u.PassphraseHash {
		t.Errorf("PassphraseHash = %q, want it unchanged (%q)", after.PassphraseHash, u.PassphraseHash)
	}

	for _, tok := range []string{phone, laptop} {
		if _, err := s.SessionUser(ctx, tok); !errors.Is(err, ErrNotFound) {
			t.Fatalf("session %q still authenticates after a credential change (%v)", tok, err)
		}
	}

	// A session minted after the reset works again, which proves the join is
	// comparing generations rather than just failing forever.
	if err := s.CreateSession(ctx, "fresh-token", after, time.Hour, "phone", ""); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := s.SessionUser(ctx, "fresh-token"); err != nil {
		t.Fatalf("a session minted after the reset does not authenticate: %v", err)
	}

	// A non-empty passphrase hash does get written, and bumps again.
	if err := s.SetCredentials(ctx, u.ID, "newer-pw", "newer-phrase"); err != nil {
		t.Fatalf("SetCredentials: %v", err)
	}
	final, err := s.UserByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if final.PassphraseHash != "newer-phrase" || final.CredentialGen != after.CredentialGen+1 {
		t.Fatalf("second SetCredentials: passphrase=%q gen=%d", final.PassphraseHash, final.CredentialGen)
	}
	if _, err := s.SessionUser(ctx, "fresh-token"); !errors.Is(err, ErrNotFound) {
		t.Fatal("the second credential change did not invalidate the session it preceded")
	}
}

func TestExpiredSessionDoesNotAuthenticate(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u := mustUser(t, s, "ada")

	// A negative TTL is an already-expired session — the same row a browser
	// holding a stale cookie would present.
	if err := s.CreateSession(ctx, "stale", u, -time.Hour, "", ""); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := s.SessionUser(ctx, "stale"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an expired session authenticated (%v)", err)
	}

	if err := s.CreateSession(ctx, "live", u, time.Hour, "", ""); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	n, err := s.PurgeExpiredSessions(ctx)
	if err != nil {
		t.Fatalf("PurgeExpiredSessions: %v", err)
	}
	if n != 1 {
		t.Fatalf("PurgeExpiredSessions removed %d rows, want 1", n)
	}
	if _, err := s.SessionUser(ctx, "live"); err != nil {
		t.Fatalf("the purge took a live session with it: %v", err)
	}
}

func TestSetDisabledBlocksAndSweepsSessions(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u := mustUser(t, s, "ada")

	if err := s.CreateSession(ctx, "tok", u, time.Hour, "", ""); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := s.SetDisabled(ctx, u.ID, true); err != nil {
		t.Fatalf("SetDisabled: %v", err)
	}
	if _, err := s.SessionUser(ctx, "tok"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a disabled user's session still authenticates (%v)", err)
	}

	// Disabling must also delete the rows, not merely fail the join: an admin
	// who disables an account expects the device list to be empty, and
	// re-enabling must not silently restore live sessions.
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id = ?`, u.ID).Scan(&n); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d session rows survived SetDisabled(true)", n)
	}

	if err := s.SetDisabled(ctx, u.ID, false); err != nil {
		t.Fatalf("SetDisabled(false): %v", err)
	}
	if _, err := s.SessionUser(ctx, "tok"); !errors.Is(err, ErrNotFound) {
		t.Fatal("re-enabling the account resurrected an old session")
	}
	back, err := s.UserByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if back.IsDisabled {
		t.Error("SetDisabled(false) did not re-enable the account")
	}
}

func TestInstances(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	ada := mustUser(t, s, "ada")
	bob := mustUser(t, s, "bob")

	if has, err := s.HasInstance(ctx, ada.ID, "workoutt"); err != nil || has {
		t.Fatalf("HasInstance before adding = %v, %v; want false, nil", has, err)
	}

	// Idempotent: the "add this app" button is a link a user can double-click,
	// and a UNIQUE violation there would be a 500 for a harmless action.
	for i := 0; i < 2; i++ {
		if err := s.AddInstance(ctx, ada.ID, "workoutt"); err != nil {
			t.Fatalf("AddInstance call %d: %v", i+1, err)
		}
	}
	if has, err := s.HasInstance(ctx, ada.ID, "workoutt"); err != nil || !has {
		t.Fatalf("HasInstance after adding = %v, %v; want true, nil", has, err)
	}

	if err := s.AddInstance(ctx, ada.ID, "readerr"); err != nil {
		t.Fatalf("AddInstance: %v", err)
	}
	if err := s.AddInstance(ctx, bob.ID, "workoutt"); err != nil {
		t.Fatalf("AddInstance: %v", err)
	}

	mine, err := s.UserInstances(ctx, ada.ID)
	if err != nil {
		t.Fatalf("UserInstances: %v", err)
	}
	if len(mine) != 2 {
		t.Fatalf("UserInstances returned %d rows, want 2 (double AddInstance created a duplicate?): %+v", len(mine), mine)
	}
	// Ordered by app name, and carrying the username the admin UI renders.
	if mine[0].App != "readerr" || mine[1].App != "workoutt" {
		t.Fatalf("UserInstances not ordered by app: %q, %q", mine[0].App, mine[1].App)
	}
	for _, in := range mine {
		if in.Username != "ada" || in.UserID != ada.ID || in.ID == 0 {
			t.Fatalf("instance row is not fully populated: %+v", in)
		}
		if in.CreatedAt.IsZero() {
			t.Fatalf("CreatedAt did not round trip: %+v", in)
		}
		if in.LastUsedAt != nil {
			t.Fatalf("a never-used instance has LastUsedAt: %+v", in)
		}
	}

	// Another user's provisioning must not leak into this one's list.
	if bobs, err := s.UserInstances(ctx, bob.ID); err != nil || len(bobs) != 1 {
		t.Fatalf("UserInstances(bob) = %d rows, %v; want 1, nil", len(bobs), err)
	}

	all, err := s.AllInstances(ctx)
	if err != nil {
		t.Fatalf("AllInstances: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("AllInstances returned %d rows, want 3", len(all))
	}
	want := []string{"ada/readerr", "ada/workoutt", "bob/workoutt"}
	for i, in := range all {
		if got := in.Username + "/" + in.App; got != want[i] {
			t.Fatalf("AllInstances[%d] = %q, want %q (ordering is username then app)", i, got, want[i])
		}
	}

	if err := s.TouchInstance(ctx, ada.ID, "workoutt"); err != nil {
		t.Fatalf("TouchInstance: %v", err)
	}
	mine, err = s.UserInstances(ctx, ada.ID)
	if err != nil {
		t.Fatalf("UserInstances: %v", err)
	}
	if mine[1].LastUsedAt == nil {
		t.Fatal("TouchInstance did not set last_used_at")
	}
	if mine[0].LastUsedAt != nil {
		t.Fatal("TouchInstance updated an app it was not asked about")
	}

	if err := s.RemoveInstance(ctx, ada.ID, "workoutt"); err != nil {
		t.Fatalf("RemoveInstance: %v", err)
	}
	if has, _ := s.HasInstance(ctx, ada.ID, "workoutt"); has {
		t.Fatal("HasInstance is still true after RemoveInstance")
	}
	// Scoped to the one user: bob still has his own workoutt.
	if has, _ := s.HasInstance(ctx, bob.ID, "workoutt"); !has {
		t.Fatal("RemoveInstance removed another user's instance of the same app")
	}

	// Deleting the account takes the provisioning rows with it (ON DELETE
	// CASCADE), otherwise AllInstances would join against a missing user and
	// silently drop or error.
	if err := s.DeleteUser(ctx, ada.ID); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	all, err = s.AllInstances(ctx)
	if err != nil {
		t.Fatalf("AllInstances after DeleteUser: %v", err)
	}
	if len(all) != 1 || all[0].Username != "bob" {
		t.Fatalf("AllInstances after DeleteUser = %+v, want just bob's", all)
	}
}

func TestThrottleEscalatesAndClears(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	const key = "login:ada"
	const free = 2
	base, max := 2*time.Second, time.Hour

	if locked, _, err := s.CheckThrottle(ctx, key); err != nil || locked {
		t.Fatalf("CheckThrottle on an untouched key = %v, %v; want false, nil", locked, err)
	}

	// The free attempts exist so a typo does not cost the user a wait.
	for i := 1; i <= free; i++ {
		until, err := s.RecordFailure(ctx, key, free, base, max)
		if err != nil {
			t.Fatalf("RecordFailure %d: %v", i, err)
		}
		if !until.IsZero() {
			t.Fatalf("RecordFailure %d (within the %d free attempts) returned a lockout at %v", i, free, until)
		}
		if locked, _, _ := s.CheckThrottle(ctx, key); locked {
			t.Fatalf("locked out after only %d failures, want %d free", i, free)
		}
	}

	first, err := s.RecordFailure(ctx, key, free, base, max)
	if err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	if first.IsZero() {
		t.Fatalf("failure %d did not lock out, want a lockout past %d free attempts", free+1, free)
	}
	locked, until, err := s.CheckThrottle(ctx, key)
	if err != nil {
		t.Fatalf("CheckThrottle: %v", err)
	}
	if !locked {
		t.Fatal("CheckThrottle reports unlocked immediately after a lockout was recorded")
	}
	if !until.After(time.Now().UTC()) {
		t.Fatalf("lockout expires at %v, which is not in the future", until)
	}

	// Doubling is the point: a slow guesser must be pushed out of reach, not
	// merely inconvenienced by a fixed delay forever.
	second, err := s.RecordFailure(ctx, key, free, base, max)
	if err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	if !second.After(first.Add(base / 2)) {
		t.Fatalf("second lockout %v is not meaningfully later than the first %v; the delay is not growing", second, first)
	}
	third, err := s.RecordFailure(ctx, key, free, base, max)
	if err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	if !third.After(second.Add(base)) {
		t.Fatalf("third lockout %v did not grow past the second %v", third, second)
	}

	if err := s.ClearThrottle(ctx, key); err != nil {
		t.Fatalf("ClearThrottle: %v", err)
	}
	if locked, _, err := s.CheckThrottle(ctx, key); err != nil || locked {
		t.Fatalf("CheckThrottle after ClearThrottle = %v, %v; want false, nil", locked, err)
	}
	// Clearing must reset the counter too, or the next single mistake would
	// land the user straight back in a long lockout.
	if until, err := s.RecordFailure(ctx, key, free, base, max); err != nil || !until.IsZero() {
		t.Fatalf("the first failure after ClearThrottle locked out at %v (%v); the counter was not reset", until, err)
	}

	// Keys are independent: one user's fumbling must not lock out another, and
	// a per-IP counter must not be confused with a per-username one.
	if locked, _, _ := s.CheckThrottle(ctx, "login:bob"); locked {
		t.Fatal("a different key is locked out")
	}
}

// The cap stops the delay running away to something indistinguishable from a
// permanent ban on an account the real owner is simply bad at typing into.
func TestThrottleRespectsMaximum(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	const key = "login:ada"
	base, max := time.Second, 3*time.Second

	var last time.Time
	for i := 0; i < 20; i++ {
		until, err := s.RecordFailure(ctx, key, 0, base, max)
		if err != nil {
			t.Fatalf("RecordFailure %d: %v", i, err)
		}
		last = until
	}
	// Twenty doublings of a second would be a fortnight if the cap (or the
	// shift-overflow guard) were missing.
	if ceiling := time.Now().UTC().Add(max + time.Minute); last.After(ceiling) {
		t.Fatalf("lockout runs to %v, past the %v maximum", last, max)
	}
	if !last.After(time.Now().UTC()) {
		t.Fatalf("lockout %v is not in the future; the shift probably overflowed to a negative duration", last)
	}
}

// A lockout that has already elapsed must read as unlocked even though the row
// still says fails=N — otherwise the counter, not the clock, would gate login.
func TestThrottleExpiresWithoutClearing(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	const key = "login:ada"
	if _, err := s.RecordFailure(ctx, key, 0, time.Hour, time.Hour); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	if locked, _, _ := s.CheckThrottle(ctx, key); !locked {
		t.Fatal("CheckThrottle did not report the lockout it was just given")
	}

	// Wind the stored deadline into the past rather than sleeping through it.
	// The counter deliberately stays at its old value: the clock, not the
	// count, is what gates the next attempt.
	if _, err := s.DB().Exec(`UPDATE throttle SET locked_until = ? WHERE key = ?`,
		time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano), key); err != nil {
		t.Fatalf("wind back the deadline: %v", err)
	}
	if locked, until, err := s.CheckThrottle(ctx, key); err != nil || locked {
		t.Fatalf("CheckThrottle on an elapsed lockout = %v (until %v), %v; want false", locked, until, err)
	}

	// A row whose locked_until is unparseable must also read as unlocked. It
	// is a corrupt record, and failing open here is the right call: failing
	// closed would lock a user out permanently with no way back.
	if _, err := s.DB().Exec(`UPDATE throttle SET locked_until = 'not a timestamp' WHERE key = ?`, key); err != nil {
		t.Fatalf("corrupt the deadline: %v", err)
	}
	if locked, _, err := s.CheckThrottle(ctx, key); err != nil || locked {
		t.Fatalf("CheckThrottle on a corrupt deadline = %v, %v; want false, nil", locked, err)
	}
}

func TestSettings(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if got := s.Setting(ctx, "signups_enabled", "fallback"); got != "fallback" {
		t.Fatalf("Setting on a missing key = %q, want the fallback", got)
	}
	if err := s.SetSetting(ctx, "signups_enabled", "true"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if got := s.Setting(ctx, "signups_enabled", "fallback"); got != "true" {
		t.Fatalf("Setting = %q, want %q", got, "true")
	}

	// Upsert, not insert: an admin toggling a setting twice must not fail on
	// the primary key.
	if err := s.SetSetting(ctx, "signups_enabled", "false"); err != nil {
		t.Fatalf("SetSetting (overwrite): %v", err)
	}
	if got := s.Setting(ctx, "signups_enabled", "fallback"); got != "false" {
		t.Fatalf("Setting after overwrite = %q, want %q", got, "false")
	}

	boolCases := []struct {
		stored   string
		set      bool
		fallback bool
		want     bool
	}{
		{stored: "true", set: true, fallback: false, want: true},
		{stored: "false", set: true, fallback: true, want: false},
		// Anything unrecognised falls back rather than guessing. A stray "1"
		// or "TRUE" must not accidentally flip signups on.
		{stored: "1", set: true, fallback: false, want: false},
		{stored: "TRUE", set: true, fallback: false, want: false},
		{stored: "yes", set: true, fallback: true, want: true},
		{stored: "", set: true, fallback: true, want: true},
		{set: false, fallback: true, want: true},
		{set: false, fallback: false, want: false},
	}
	for i, tc := range boolCases {
		key := "bool_case"
		if tc.set {
			if err := s.SetSetting(ctx, key, tc.stored); err != nil {
				t.Fatalf("SetSetting: %v", err)
			}
		} else {
			key = "never_set_key"
		}
		if got := s.BoolSetting(ctx, key, tc.fallback); got != tc.want {
			t.Errorf("case %d: BoolSetting(stored=%q set=%v fallback=%v) = %v, want %v",
				i, tc.stored, tc.set, tc.fallback, got, tc.want)
		}
	}
}

func TestAuditIsNewestFirstAndLimited(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	actions := []string{"login", "logout", "password_reset", "instance_add", "user_disable"}
	for _, a := range actions {
		if err := s.Audit(ctx, "ada", a, "ada", "detail for "+a, "203.0.113.7"); err != nil {
			t.Fatalf("Audit(%q): %v", a, err)
		}
	}

	// Newest first. The rows are written within the same millisecond, so this
	// only holds because the ordering is by insertion id rather than by the
	// timestamp text — worth pinning, because ordering by `at` would look
	// correct and shuffle in production.
	got, err := s.RecentAudit(ctx, 10)
	if err != nil {
		t.Fatalf("RecentAudit: %v", err)
	}
	if len(got) != len(actions) {
		t.Fatalf("RecentAudit returned %d entries, want %d", len(got), len(actions))
	}
	for i, e := range got {
		want := actions[len(actions)-1-i]
		if e.Action != want {
			t.Fatalf("RecentAudit[%d].Action = %q, want %q (newest first)", i, e.Action, want)
		}
	}

	first := got[0]
	if first.Actor != "ada" || first.Target != "ada" || first.IP != "203.0.113.7" || first.Detail != "detail for user_disable" {
		t.Fatalf("audit fields did not round trip: %+v", first)
	}
	if first.At.IsZero() {
		t.Fatalf("audit timestamp did not parse: %+v", first)
	}

	limited, err := s.RecentAudit(ctx, 2)
	if err != nil {
		t.Fatalf("RecentAudit(2): %v", err)
	}
	if len(limited) != 2 {
		t.Fatalf("RecentAudit(2) returned %d entries", len(limited))
	}
	if limited[0].Action != "user_disable" || limited[1].Action != "instance_add" {
		t.Fatalf("RecentAudit(2) = %q, %q; want the two newest", limited[0].Action, limited[1].Action)
	}

	// Audit rows outlive the user they describe — that is the point of a trail.
	empty, err := s.RecentAudit(ctx, 0)
	if err != nil {
		t.Fatalf("RecentAudit(0): %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("RecentAudit(0) returned %d entries, want none", len(empty))
	}
}

func TestListAndCountUsers(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if n, err := s.CountUsers(ctx); err != nil || n != 0 {
		t.Fatalf("CountUsers on an empty database = %d, %v; want 0, nil", n, err)
	}
	// CountUsers == 0 is what the first-run bootstrap keys off, so an empty
	// ListUsers must not be an error either.
	if list, err := s.ListUsers(ctx); err != nil || len(list) != 0 {
		t.Fatalf("ListUsers on an empty database = %v, %v", list, err)
	}

	for _, name := range []string{"zoe", "ada", "bob"} {
		mustUser(t, s, name)
	}
	list, err := s.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	got := make([]string, len(list))
	for i, u := range list {
		got[i] = u.Username
	}
	if len(got) != 3 || got[0] != "ada" || got[1] != "bob" || got[2] != "zoe" {
		t.Fatalf("ListUsers = %v, want alphabetical [ada bob zoe]", got)
	}
	if n, err := s.CountUsers(ctx); err != nil || n != 3 {
		t.Fatalf("CountUsers = %d, %v; want 3, nil", n, err)
	}
}

func TestSetAdminAndTouchLogin(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u := mustUser(t, s, "ada")

	if err := s.SetAdmin(ctx, u.ID, true); err != nil {
		t.Fatalf("SetAdmin: %v", err)
	}
	after, err := s.UserByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if !after.IsAdmin {
		t.Fatal("SetAdmin(true) did not stick")
	}
	// Promotion must not disturb credentials — a bug here would log the user
	// out (or worse, not) as a side effect of an unrelated admin action.
	if after.CredentialGen != u.CredentialGen || after.PasswordHash != u.PasswordHash {
		t.Fatalf("SetAdmin changed credentials: gen %d->%d", u.CredentialGen, after.CredentialGen)
	}

	if err := s.TouchLogin(ctx, u.ID); err != nil {
		t.Fatalf("TouchLogin: %v", err)
	}
	after, err = s.UserByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if after.LastLoginAt == nil {
		t.Fatal("TouchLogin did not record a login time")
	}
	if d := time.Since(*after.LastLoginAt); d < 0 || d > time.Minute {
		t.Fatalf("LastLoginAt is %v away from now; the timestamp round trip is wrong", d)
	}
}

func TestDeleteUserCascadesSessions(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u := mustUser(t, s, "ada")

	if err := s.CreateSession(ctx, "tok", u, time.Hour, "", ""); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := s.DeleteUser(ctx, u.ID); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if _, err := s.SessionUser(ctx, "tok"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a deleted user's session still resolves (%v)", err)
	}
	// The cascade needs foreign_keys(ON) on the connection; without it the row
	// would linger and SessionUser would only fail because of the join.
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d orphaned session rows survived DeleteUser; foreign keys are not enforced", n)
	}
	if _, err := s.UserByName(ctx, "ada"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UserByName after DeleteUser = %v, want ErrNotFound", err)
	}
}

func TestHashTokenIsStableAndDistinct(t *testing.T) {
	a := HashToken("token-a")
	if a != HashToken("token-a") {
		t.Fatal("HashToken is not deterministic; every session would be lost on the next request")
	}
	if a == HashToken("token-b") {
		t.Fatal("HashToken collided on two different tokens")
	}
	if len(a) != 64 {
		t.Fatalf("HashToken produced %d hex characters, want 64 (SHA-256)", len(a))
	}
	if a == "token-a" {
		t.Fatal("HashToken returned its input")
	}
}

func TestPurgeStaleThrottles(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	// A live lockout must survive a purge, however old the row is; dropping it
	// would hand an attacker a reset by simply waiting for the sweeper.
	if _, err := s.RecordFailure(ctx, "login:ada", 0, time.Hour, time.Hour); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	if err := s.PurgeStaleThrottles(ctx, 0); err != nil {
		t.Fatalf("PurgeStaleThrottles: %v", err)
	}
	if locked, _, _ := s.CheckThrottle(ctx, "login:ada"); !locked {
		t.Fatal("PurgeStaleThrottles dropped a row that is still locked out")
	}

	// A row with no lockout at all and an old timestamp is exactly what the
	// sweeper is for.
	if _, err := s.RecordFailure(ctx, "login:bob", 5, time.Hour, time.Hour); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	if _, err := s.DB().Exec(`UPDATE throttle SET updated_at = ? WHERE key = ?`,
		time.Now().UTC().Add(-48*time.Hour).Format(time.RFC3339Nano), "login:bob"); err != nil {
		t.Fatalf("age the row: %v", err)
	}
	if err := s.PurgeStaleThrottles(ctx, 24*time.Hour); err != nil {
		t.Fatalf("PurgeStaleThrottles: %v", err)
	}
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM throttle WHERE key = ?`, "login:bob").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatal("PurgeStaleThrottles left a stale, unlocked counter behind")
	}
}
