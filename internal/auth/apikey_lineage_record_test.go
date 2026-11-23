// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// lineageDenier fails every command naming an `apikey-lineage:` key, like a lockdown ACL without
// `~apikey-lineage:*` (NOPERM) or a Redis refusing writes (failWith, e.g. OOM); it counts each refusal.
type lineageDenier struct {
	failWith atomic.Pointer[string]
	denied   atomic.Int64
	expires  atomic.Int64 // EXPIRE-family commands seen, any key
}

func (d *lineageDenier) reject(cmd redis.Cmder) error {
	if strings.HasPrefix(strings.ToLower(cmd.Name()), "expire") {
		d.expires.Add(1)
	}
	msg := d.failWith.Load()
	if msg == nil {
		return nil
	}
	for _, a := range cmd.Args() {
		if s, ok := a.(string); ok && strings.HasPrefix(s, "apikey-lineage:") {
			d.denied.Add(1)
			err := errors.New(*msg)
			cmd.SetErr(err)
			return err
		}
	}
	return nil
}

func (d *lineageDenier) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) { return next(ctx, network, addr) }
}

func (d *lineageDenier) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if err := d.reject(cmd); err != nil {
			return err
		}
		return next(ctx, cmd)
	}
}

func (d *lineageDenier) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		var first error
		for _, cmd := range cmds {
			if err := d.reject(cmd); err != nil && first == nil {
				first = err
			}
		}
		if first != nil {
			return first
		}
		return next(ctx, cmds)
	}
}

type lineageRecFixture struct {
	mr     *miniredis.Miniredis
	rdb    *redis.Client
	store  *RedisAPIKeyStore
	v      *RedisAPIKeyValidator
	denier *lineageDenier
	now    time.Time
}

func newLineageRecFixture(t *testing.T) *lineageRecFixture {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	f := &lineageRecFixture{mr: mr, rdb: rdb, denier: &lineageDenier{}, now: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
	rdb.AddHook(f.denier)
	clock := func() time.Time { return f.now }
	f.store = NewRedisAPIKeyStore(rdb, WithStoreClock(clock))
	f.v = NewRedisAPIKeyValidator(rdb, WithClock(clock))
	return f
}

func (f *lineageRecFixture) advance(d time.Duration) {
	f.now = f.now.Add(d)
	f.mr.FastForward(d)
}

// mint creates a key without authenticating it, so no slide happens at mint.
func (f *lineageRecFixture) mint(t *testing.T, req CreateAPIKeyRequest) (string, Subject) {
	t.Helper()
	rec, plain, err := f.store.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sub, err := subjectFromRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	return hashAPIKey(plain), sub
}

func (f *lineageRecFixture) lineageExists(hash string) bool {
	return f.mr.Exists(cachekeys.APIKeyLineage(hash).String())
}

func TestMint_WritesLineageRecordWithCredentialLifetime(t *testing.T) {
	f := newLineageRecFixture(t)
	opHash, opSub := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-x"})
	ssHash, _ := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-x", SelfService: true})

	raw, err := f.mr.Get(cachekeys.APIKeyLineage(opHash).String())
	if err != nil {
		t.Fatalf("operator key has no lineage record: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"key_id": opSub.KeyID, "identifier": "acct-x"}; !reflect.DeepEqual(got, want) {
		t.Errorf("lineage record fields = %v, want exactly key_id and identifier", got)
	}
	if ttl := f.mr.TTL(cachekeys.APIKeyLineage(opHash).String()); ttl != 0 {
		t.Errorf("persistent credential's lineage TTL = %v, want persistent", ttl)
	}
	if ttl := f.mr.TTL(cachekeys.APIKeyLineage(ssHash).String()); ttl != MirroredKeyIdleTTL+LineageTTLMargin {
		t.Errorf("self-service lineage TTL = %v, want %v", ttl, MirroredKeyIdleTTL+LineageTTLMargin)
	}
}

// Invariants 2 and 3: a child in daily use keeps its idle root's lineage record alive past the root's
// credential, and the root's credential expires exactly when it would with no lineage at all.
func TestLineage_DescendantUseOutlivesIdleAncestorCredential(t *testing.T) {
	f := newLineageRecFixture(t)
	rootHash, rootSub := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-x", SelfService: true})
	midHash, midSub := f.mint(t, ChildKeyRequest(rootSub, "mid", nil))
	_, leafPlain, err := f.store.Create(context.Background(), ChildKeyRequest(midSub, "leaf", nil))
	if err != nil {
		t.Fatal(err)
	}
	rootCred := cachekeys.APIKey(rootHash).String()
	for day := 1; day <= 120; day++ {
		f.advance(24 * time.Hour)
		if _, err := f.v.Lookup(context.Background(), leafPlain); err != nil {
			t.Fatalf("day %d: leaf Lookup: %v", day, err)
		}
		if day == 45 {
			if ttl := f.mr.TTL(rootCred); ttl != MirroredKeyIdleTTL-45*24*time.Hour {
				t.Fatalf("day 45: root credential TTL = %v, want %v (a descendant must not touch it)", ttl, MirroredKeyIdleTTL-45*24*time.Hour)
			}
		}
		if day == 90 && f.mr.Exists(rootCred) {
			t.Fatal("day 90: idle root credential still exists, want expired on its own idle window")
		}
	}
	for name, hash := range map[string]string{"root": rootHash, "mid": midHash} {
		if f.mr.Exists(cachekeys.APIKey(hash).String()) {
			t.Errorf("day 120: idle %s credential still exists", name)
		}
		if stands, err := f.v.AncestorStands(context.Background(), hash); err != nil || !stands {
			t.Errorf("day 120: AncestorStands(%s) = %v, %v; want true while its descendant is in daily use", name, stands, err)
		}
	}
}

func TestAncestorStands_CredentialOrLineage(t *testing.T) {
	f := newLineageRecFixture(t)
	hash, _ := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-x", SelfService: true})
	ctx := context.Background()
	if ok, err := f.v.AncestorStands(ctx, hash); err != nil || !ok {
		t.Fatalf("both records: %v, %v; want true", ok, err)
	}
	f.mr.Del(cachekeys.APIKey(hash).String())
	if ok, err := f.v.AncestorStands(ctx, hash); err != nil || !ok {
		t.Fatalf("lineage only: %v, %v; want true", ok, err)
	}
	f.mr.Del(cachekeys.APIKeyLineage(hash).String())
	if ok, err := f.v.AncestorStands(ctx, hash); err != nil || ok {
		t.Fatalf("neither: %v, %v; want false", ok, err)
	}
	nope := "NOPERM No permissions to access a key"
	f.denier.failWith.Store(&nope)
	if ok, err := f.v.AncestorStands(ctx, hash); err == nil || ok {
		t.Fatalf("ACL denied: %v, %v; want an error, never true", ok, err)
	}
}

func TestLineage_SlideIsThrottledPerAuthenticatingKey(t *testing.T) {
	f := newLineageRecFixture(t)
	_, rootSub := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-x", SelfService: true})
	_, plain, err := f.store.Create(context.Background(), ChildKeyRequest(rootSub, "c", nil))
	if err != nil {
		t.Fatal(err)
	}
	lookups := func(n int) int64 {
		before := f.denier.expires.Load()
		for range n {
			if _, err := f.v.Lookup(context.Background(), plain); err != nil {
				t.Fatal(err)
			}
		}
		return f.denier.expires.Load() - before
	}
	// One credential refresh per Lookup, plus one slide (the key, its root and their owner set) per hour.
	if got := lookups(10); got != 10+3 {
		t.Errorf("10 Lookups in one hour sent %d EXPIREs, want 13", got)
	}
	f.advance(lineageSlideInterval)
	if got := lookups(1); got != 1+3 {
		t.Errorf("first Lookup after the interval sent %d EXPIREs, want 4", got)
	}
}

// A failed slide is retried a minute later, not an hour later, and not on every request in between.
func TestLineage_FailedSlideRetriesAfterAMinute(t *testing.T) {
	f := newLineageRecFixture(t)
	_, rootSub := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-x", SelfService: true})
	_, plain, err := f.store.Create(context.Background(), ChildKeyRequest(rootSub, "c", nil))
	if err != nil {
		t.Fatal(err)
	}
	oom := "OOM command not allowed when used memory > 'maxmemory'."
	f.denier.failWith.Store(&oom)
	lookup := func() int64 {
		before := f.denier.denied.Load()
		if _, err := f.v.Lookup(context.Background(), plain); err != nil {
			t.Fatal(err)
		}
		return f.denier.denied.Load() - before
	}
	if lookup() == 0 {
		t.Fatal("first Lookup attempted no slide")
	}
	f.advance(30 * time.Second)
	if lookup() != 0 {
		t.Error("a slide was retried within the minute: a Redis outage would see one per request")
	}
	f.advance(31 * time.Second)
	if lookup() == 0 {
		t.Error("no slide retried a minute after the failure")
	}
	f.denier.failWith.Store(nil)
	f.advance(time.Minute)
	if before := f.denier.expires.Load(); lookup() == 0 && f.denier.expires.Load()-before != 1+3 {
		t.Error("the slide after Redis recovered did not run")
	}
}

// Invariant 4: revoke and erasure delete the lineage record, and a descendant's in-flight slide cannot
// bring it back.
func TestLineage_RevokeAndErasureDeleteAndTrafficCannotRecreate(t *testing.T) {
	f := newLineageRecFixture(t)
	rootHash, rootSub := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-x", SelfService: true})
	childHash, childSub := f.mint(t, ChildKeyRequest(rootSub, "c", nil))
	otherHash, _ := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-y", SelfService: true})
	childRec, _, err := f.store.getRecord(context.Background(), childHash)
	if err != nil {
		t.Fatal(err)
	}

	if err := f.store.RevokeKeyByID(context.Background(), "acct-x", rootSub.KeyID); err != nil {
		t.Fatal(err)
	}
	f.advance(2 * lineageSlideInterval)
	f.v.lineage.slide(context.Background(), f.rdb, childHash, childRec, MirroredKeyIdleTTL, f.now)
	for name, h := range map[string]string{"root": rootHash, "child": childHash} {
		if f.lineageExists(h) {
			t.Errorf("%s lineage record outlived the revoke", name)
		}
		if stands, err := f.v.AncestorStands(context.Background(), h); err != nil || stands {
			t.Errorf("AncestorStands(%s) after revoke = %v, %v; want false", name, stands, err)
		}
	}
	_ = childSub

	if _, err := f.store.DeleteKeysForIdentifier(context.Background(), "acct-y"); err != nil {
		t.Fatal(err)
	}
	if f.lineageExists(otherHash) {
		t.Error("lineage record outlived account erasure's key delete")
	}
}

// Invariant 1 under total lineage failure: every key state authenticates exactly as with lineage
// working, and mint, revoke and erasure still succeed. The denial count proves the failures happened.
func TestLineage_FailuresNeverChangeAnAuthOutcome(t *testing.T) {
	states := map[string]func(*APIKeyRecord){
		"active":  func(*APIKeyRecord) {},
		"revoked": func(r *APIKeyRecord) { r.RevokedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
		"expired": func(r *APIKeyRecord) { r.ExpiresAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
		"delegated": func(r *APIKeyRecord) {
			r.ParentKeyID, r.AncestorKeyIDs, r.AncestorHashes = "kid_p", []string{"kid_p"}, []string{"ab"}
		},
		"legacy":      func(r *APIKeyRecord) { r.ParentKeyID = "kid_p" },
		"no key id":   func(r *APIKeyRecord) { r.KeyID = "" },
		"idle-absent": nil,
	}
	// Written out, not derived: a change that moved both runs alike still fails here.
	expected := map[string]error{
		"active": nil, "delegated": nil, "legacy": nil,
		"revoked": ErrUnauthorized, "expired": ErrTokenExpired, "idle-absent": ErrUnauthorized,
		"no key id": errNoKeyIDOutcome,
	}
	type outcome struct {
		sub Subject
		err string
		ttl time.Duration
	}
	run := func(t *testing.T, failWith string, withSlider bool) map[string]outcome {
		f := newLineageRecFixture(t)
		if !withSlider {
			f.v.lineage = nil
		}
		if failWith != "" {
			f.denier.failWith.Store(&failWith)
		}
		out := map[string]outcome{}
		for name, mutate := range states {
			for _, ttl := range []time.Duration{0, MirroredKeyIdleTTL} {
				plain := "sip_" + strings.ReplaceAll(name, " ", "-") + ttl.String()
				key := cachekeys.APIKey(hashAPIKey(plain)).String()
				if mutate != nil {
					rec := APIKeyRecord{KeyID: "kid_" + name, Identifier: "acct-x", PermissionsAll: true}
					mutate(&rec)
					body, _ := json.Marshal(rec)
					if err := f.store.writeRecord(context.Background(), hashAPIKey(plain), rec, body, ttl); err != nil {
						t.Fatal(err)
					}
				}
				f.advance(time.Minute)
				sub, err := f.v.Lookup(context.Background(), plain)
				o := outcome{sub: sub, ttl: f.mr.TTL(key)}
				if err != nil {
					o.err = err.Error()
				}
				want := expected[name]
				switch {
				case want == nil && (err != nil || sub.KeyID == ""):
					t.Errorf("%s/%v (%q): got %v, want allowed", name, ttl, failWith, err)
				case errors.Is(want, errNoKeyIDOutcome) && (err == nil || !strings.Contains(err.Error(), "no key id")):
					t.Errorf("%s/%v (%q): got %v, want rejected for a missing key id", name, ttl, failWith, err)
				case want != nil && !errors.Is(want, errNoKeyIDOutcome) && !errors.Is(err, want):
					t.Errorf("%s/%v (%q): got %v, want %v", name, ttl, failWith, err, want)
				}
				out[name+"/"+ttl.String()] = o
			}
		}
		_, rootSub := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-z", SelfService: true})
		_, childPlain, err := f.store.Create(context.Background(), ChildKeyRequest(rootSub, "c", nil))
		if err != nil {
			t.Fatalf("delegated mint with lineage failing: %v", err)
		}
		if _, err := f.v.Lookup(context.Background(), childPlain); err != nil {
			t.Fatalf("child Lookup with lineage failing: %v", err)
		}
		removed, err := f.store.RevokeKeyTree(context.Background(), "acct-z", rootSub.KeyID)
		if err != nil || len(removed) != 1 {
			t.Fatalf("revoke (%q): removed %v, err %v; want the child removed and no error", failWith, removed, err)
		}
		if _, err := f.v.Lookup(context.Background(), childPlain); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("revoked child Lookup (%q): %v, want ErrUnauthorized", failWith, err)
		}
		if deleted, err := f.store.DeleteKeysForIdentifier(context.Background(), "acct-x"); err != nil || len(deleted) != 12 {
			t.Fatalf("erasure delete (%q): deleted %d, err %v; want 12 and no error", failWith, len(deleted), err)
		}
		if failWith != "" && f.denier.denied.Load() == 0 {
			t.Fatal("no lineage command was refused: the failure path was not exercised")
		}
		return out
	}
	oracle := run(t, "", false)
	if got := run(t, "", true); !reflect.DeepEqual(got, oracle) {
		t.Errorf("auth outcomes with the slider differ from without it:\n got %+v\nwant %+v", got, oracle)
	}
	for _, failWith := range []string{
		"NOPERM No permissions to access a key",
		"OOM command not allowed when used memory > 'maxmemory'.",
		"i/o timeout",
	} {
		before := testutil.ToFloat64(obs.APIKeyLineageWriteFailuresTotal.WithLabelValues(obs.APIKeyLineageOpSlide))
		got := run(t, failWith, true)
		if !reflect.DeepEqual(got, oracle) {
			t.Errorf("%s: auth outcomes differ from no lineage slider:\n got %+v\nwant %+v", failWith, got, oracle)
		}
		if after := testutil.ToFloat64(obs.APIKeyLineageWriteFailuresTotal.WithLabelValues(obs.APIKeyLineageOpSlide)); after <= before {
			t.Errorf("%s: slide failures not counted", failWith)
		}
	}
}

// errNoKeyIDOutcome stands in the expected-outcome table for Lookup's non-sentinel missing-key-id rejection.
var errNoKeyIDOutcome = errors.New("rejected: no key id")

func TestBackfill_CreatesMissingLineageRecordsIdempotently(t *testing.T) {
	f := newLineageRecFixture(t)
	rootHash, _ := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-x", SelfService: true})
	opHash, _ := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-x"})
	f.mr.Del(cachekeys.APIKeyLineage(rootHash).String())
	f.mr.Del(cachekeys.APIKeyLineage(opHash).String())
	f.advance(time.Hour)

	rep, err := f.store.BackfillAncestorHashes(context.Background(), false)
	if err != nil || len(rep.LineageMissing) != 2 || f.lineageExists(rootHash) {
		t.Fatalf("dry run: missing %v, err %v; want 2 reported, none written", rep.LineageMissing, err)
	}
	if _, err := f.store.BackfillAncestorHashes(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if ttl := f.mr.TTL(cachekeys.APIKeyLineage(rootHash).String()); ttl != MirroredKeyIdleTTL-time.Hour+LineageTTLMargin {
		t.Errorf("backfilled lineage TTL = %v, want the credential's remaining TTL plus the margin", ttl)
	}
	if ttl := f.mr.TTL(cachekeys.APIKeyLineage(opHash).String()); !f.lineageExists(opHash) || ttl != 0 {
		t.Errorf("persistent key's backfilled lineage: exists %v, TTL %v; want persistent", f.lineageExists(opHash), ttl)
	}
	if rep, err := f.store.BackfillAncestorHashes(context.Background(), false); err != nil || len(rep.LineageMissing) != 0 {
		t.Fatalf("dry run after write: missing %v, err %v; want 0", rep.LineageMissing, err)
	}

	nope := "NOPERM No permissions to access a key"
	f.denier.failWith.Store(&nope)
	if rep, err := f.store.BackfillAncestorHashes(context.Background(), false); err != nil || !rep.LineageDenied {
		t.Fatalf("backfill under a missing ACL grant: denied %v, err %v; want reported denied", rep.LineageDenied, err)
	}
}

// The backfill's orphan sweep deletes lineage records with no credential that no credential's chain lists,
// keeps one a live descendant holds, reports without deleting on a dry run, and finds nothing on a rerun.
func TestBackfill_SweepsOrphanedLineageRecords(t *testing.T) {
	ctx := context.Background()
	f := newLineageRecFixture(t)
	heldHash, heldSub := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-x", SelfService: true})
	_, childPlain, err := f.store.Create(ctx, ChildKeyRequest(heldSub, "c", nil))
	if err != nil {
		t.Fatal(err)
	}
	orphanHash, _ := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-y"}) // persistent: no TTL would ever clear it
	liveHash, _ := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-z"})
	f.useDaily(t, childPlain, 100)
	// An old binary's revoke: the credential goes, the lineage record stays.
	f.mr.Del(cachekeys.APIKey(orphanHash).String())
	if !f.lineageExists(heldHash) || !f.lineageExists(orphanHash) {
		t.Fatal("setup: want both lineage records standing")
	}
	var lineage []string
	for _, k := range f.mr.Keys() {
		if h, ok := strings.CutPrefix(k, "apikey-lineage:"); ok && !strings.HasPrefix(h, "owner:") {
			lineage = append(lineage, h, h) // SCAN may return a key twice
		}
	}
	rep, err := f.store.BackfillAncestorHashes(ctx, false, lineage...)
	if err != nil || rep.LineageOrphans != 1 || rep.LineageOrphansDeleted != 0 || !f.lineageExists(orphanHash) {
		t.Fatalf("dry run: orphans %d, deleted %d, err %v; want 1 reported and none deleted", rep.LineageOrphans, rep.LineageOrphansDeleted, err)
	}
	if rep, err = f.store.BackfillAncestorHashes(ctx, true, lineage...); err != nil || rep.LineageOrphansDeleted != 1 {
		t.Fatalf("write: deleted %d, err %v; want 1", rep.LineageOrphansDeleted, err)
	}
	if f.lineageExists(orphanHash) || !f.lineageExists(heldHash) || !f.lineageExists(liveHash) {
		t.Errorf("after write: orphan %v, held %v, live %v; want only the orphan gone",
			f.lineageExists(orphanHash), f.lineageExists(heldHash), f.lineageExists(liveHash))
	}
	if rep, err = f.store.BackfillAncestorHashes(ctx, true, lineage...); err != nil || rep.LineageOrphansDeleted != 0 {
		t.Fatalf("rerun: deleted %d, err %v; want 0", rep.LineageOrphansDeleted, err)
	}
}

// cmdFailer fails, with OOM, every command (alone or pipelined) that fail selects.
type cmdFailer struct{ fail func(cmd redis.Cmder) bool }

func (c cmdFailer) err(cmd redis.Cmder) error {
	if !c.fail(cmd) {
		return nil
	}
	err := errors.New("OOM command not allowed when used memory > 'maxmemory'.")
	cmd.SetErr(err)
	return err
}

func (c cmdFailer) DialHook(next redis.DialHook) redis.DialHook { return next }

func (c cmdFailer) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if err := c.err(cmd); err != nil {
			return err
		}
		return next(ctx, cmd)
	}
}

func (c cmdFailer) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		var first error
		for _, cmd := range cmds {
			if err := c.err(cmd); err != nil && first == nil {
				first = err
			}
		}
		if first != nil {
			return first
		}
		return next(ctx, cmds)
	}
}

// names reports whether cmd is named (case-insensitively) one of names and has an argument with prefix.
func names(cmd redis.Cmder, prefix string, names ...string) bool {
	if !slices.Contains(names, strings.ToLower(cmd.Name())) {
		return false
	}
	return slices.ContainsFunc(cmd.Args(), func(a any) bool { s, ok := a.(string); return ok && strings.HasPrefix(s, prefix) })
}

// A revoke's outcome is its credential deletes' alone: a lineage delete that fails, before or after the
// credential is gone, neither fails the revoke nor drops a key from the removed list.
func TestRevoke_LineageDeleteFailureNeverChangesTheOutcome(t *testing.T) {
	for name, gone := range map[string]bool{"while the credential exists": false, "after the credential is gone": true} {
		t.Run(name, func(t *testing.T) {
			f := newLineageRecFixture(t)
			_, rootSub := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-x", SelfService: true})
			childHash, childSub := f.mint(t, ChildKeyRequest(rootSub, "c", nil))
			f.rdb.AddHook(cmdFailer{fail: func(cmd redis.Cmder) bool {
				return names(cmd, "apikey-lineage:"+childHash, "del") && f.mr.Exists(cachekeys.APIKey(childHash).String()) != gone
			}})
			removed, err := f.store.RevokeKeyTree(context.Background(), "acct-x", rootSub.KeyID)
			if err != nil || !slices.Equal(removed, []string{childSub.KeyID}) {
				t.Fatalf("removed %v, err %v; want the child removed and no error", removed, err)
			}
			if f.mr.Exists(cachekeys.APIKey(childHash).String()) {
				t.Error("child credential survived the revoke")
			}
		})
	}
}

// A credential delete that fails still fails the revoke, and the key still authenticates for a retry.
func TestRevoke_CredentialDeleteFailureStillFailsTheRevoke(t *testing.T) {
	f := newLineageRecFixture(t)
	rootHash, rootSub := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-x", SelfService: true})
	f.rdb.AddHook(cmdFailer{fail: func(cmd redis.Cmder) bool {
		return names(cmd, cachekeys.APIKey(rootHash).String(), "evalsha", "eval", "del")
	}})
	err := f.store.RevokeKeyByID(context.Background(), "acct-x", rootSub.KeyID)
	if err == nil || !strings.Contains(err.Error(), "redis del "+cachekeys.APIKey(rootHash).String()) {
		t.Fatalf("revoke: %v; want the credential delete's error", err)
	}
	if !f.mr.Exists(cachekeys.APIKey(rootHash).String()) {
		t.Fatal("credential gone although its delete failed")
	}
}

// removesCredential reports whether cmd is the indexed-record removal of the credential at hash.
func removesCredential(cmd redis.Cmder, hash string) bool {
	return names(cmd, cachekeys.APIKeyIndex().String(), "evalsha", "eval") &&
		slices.Contains(cmd.Args(), any(cachekeys.APIKey(hash).String()))
}

// A revoke that fails part-way, at a middle key or at the root, keeps the lineage record of every key whose
// credential still exists; the retry that succeeds deletes them all, and the owner set with them.
func TestRevoke_PartialFailureKeepsLineageOfLiveKeys(t *testing.T) {
	for _, failAt := range []string{"mid", "root"} {
		t.Run(failAt, func(t *testing.T) {
			ctx := context.Background()
			f := newLineageRecFixture(t)
			rootHash, rootSub := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-x", SelfService: true})
			midHash, midSub := f.mint(t, ChildKeyRequest(rootSub, "mid", nil))
			leafHash, _ := f.mint(t, ChildKeyRequest(midSub, "leaf", nil))
			target := map[string]string{"mid": midHash, "root": rootHash}[failAt]
			var armed atomic.Bool
			armed.Store(true)
			f.rdb.AddHook(cmdFailer{fail: func(cmd redis.Cmder) bool { return armed.Load() && removesCredential(cmd, target) }})
			if err := f.store.RevokeKeyByID(ctx, "acct-x", rootSub.KeyID); err == nil {
				t.Fatal("revoke with a failing credential delete returned nil")
			}
			for name, h := range map[string]string{"root": rootHash, "mid": midHash, "leaf": leafHash} {
				if f.mr.Exists(cachekeys.APIKey(h).String()) && !f.lineageExists(h) {
					t.Errorf("%s: credential stands but its lineage record was deleted", name)
				}
			}
			armed.Store(false)
			if err := f.store.RevokeKeyByID(ctx, "acct-x", rootSub.KeyID); err != nil {
				t.Fatalf("retry: %v", err)
			}
			if left, set := f.lineageOf(t, "acct-x"); len(left) != 0 || set {
				t.Errorf("after the retry: %d lineage records, owner set %v; want none", len(left), set)
			}
		})
	}
}

// Erasure walks the owner set with SSCAN, never SMEMBERS, and across more than one batch drops every member
// whose key is gone (with or without a lineage record) while a key whose credential exists keeps both.
func TestErasure_WalksTheOwnerSetInBatchesAndSparesLiveKeys(t *testing.T) {
	ctx := context.Background()
	f := newLineageRecFixture(t)
	liveHash, _ := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-x", SelfService: true})
	owner := cachekeys.APIKeyLineageOwner("acct-x").String()
	body, _ := json.Marshal(lineageRecord{KeyID: "kid_old", Identifier: "acct-x"})
	for i := range 3*lineageDeleteBatch + 7 {
		h := fmt.Sprintf("stale%04d", i)
		if _, err := f.mr.SAdd(owner, h); err != nil {
			t.Fatal(err)
		}
		if i%2 == 0 {
			if err := f.mr.Set(cachekeys.APIKeyLineage(h).String(), string(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	var seen []string
	f.rdb.AddHook(cmdFailer{fail: func(cmd redis.Cmder) bool { seen = append(seen, strings.ToLower(cmd.Name())); return false }})
	if !deleteAccountLineage(ctx, f.rdb, "acct-x") {
		t.Fatal("deleteAccountLineage reported a failure")
	}
	pages := 0
	for _, c := range seen {
		if c == "evalsha" || c == "eval" {
			pages++
		}
	}
	if slices.Contains(seen, "smembers") || pages < 2 {
		t.Errorf("commands %v; want the set paged by script calls and no SMEMBERS", seen)
	}
	if left, _ := f.lineageOf(t, "acct-x"); !slices.Equal(left, []string{liveHash}) {
		t.Errorf("lineage records left %d; want only the live key's", len(left))
	}
	if members, err := f.mr.Members(owner); err != nil || !slices.Equal(members, []string{liveHash}) {
		t.Errorf("owner set has %d members (err %v); want only the live key", len(members), err)
	}
}

// lineageOf returns the hashes of every lineage record naming identifier, and whether its owner set exists.
func (f *lineageRecFixture) lineageOf(t *testing.T, identifier string) ([]string, bool) {
	t.Helper()
	var out []string
	for _, k := range f.mr.Keys() {
		h, ok := strings.CutPrefix(k, "apikey-lineage:")
		if !ok || strings.HasPrefix(h, "owner:") {
			continue
		}
		body, err := f.mr.Get(k)
		if err != nil {
			t.Fatal(err)
		}
		var rec lineageRecord
		if err := json.Unmarshal([]byte(body), &rec); err != nil {
			t.Fatal(err)
		}
		if rec.Identifier == identifier {
			out = append(out, h)
		}
	}
	return out, f.mr.Exists(cachekeys.APIKeyLineageOwner(identifier).String())
}

// useDaily authenticates plain once a day for days, idling out every key in its chain that is not used.
func (f *lineageRecFixture) useDaily(t *testing.T, plain string, days int) {
	t.Helper()
	for range days {
		f.advance(24 * time.Hour)
		if _, err := f.v.Lookup(context.Background(), plain); err != nil {
			t.Fatal(err)
		}
	}
}

// Erasure leaves no lineage record holding the account's identifier, whether its key's credential idled
// out under a live child or every credential of the account is gone; another account's records stand.
func TestErasure_DeletesLineageOfIdledOutKeys(t *testing.T) {
	t.Run("idle root under a live child", func(t *testing.T) {
		ctx := context.Background()
		f := newLineageRecFixture(t)
		rootHash, rootSub := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-x", SelfService: true})
		_, childPlain, err := f.store.Create(ctx, ChildKeyRequest(rootSub, "c", nil))
		if err != nil {
			t.Fatal(err)
		}
		f.useDaily(t, childPlain, 100)
		if f.mr.Exists(cachekeys.APIKey(rootHash).String()) || !f.lineageExists(rootHash) {
			t.Fatal("setup: want the root's credential idled out and its lineage record standing")
		}
		if _, err := f.store.DeleteKeysForIdentifier(ctx, "acct-x"); err != nil {
			t.Fatal(err)
		}
		if left, set := f.lineageOf(t, "acct-x"); len(left) != 0 || set {
			t.Errorf("after erasure: %d lineage records name the account, owner set %v; want none", len(left), set)
		}
	})
	t.Run("no credential left", func(t *testing.T) {
		ctx := context.Background()
		f := newLineageRecFixture(t)
		_, rootSub := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-x", SelfService: true})
		f.mint(t, ChildKeyRequest(rootSub, "c", nil))
		otherHash, _ := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-y"})
		f.advance(MirroredKeyIdleTTL + time.Hour)
		if left, _ := f.lineageOf(t, "acct-x"); len(left) != 2 {
			t.Fatalf("setup: %d lineage records outlive the credentials, want 2", len(left))
		}
		if deleted, err := f.store.DeleteKeysForIdentifier(ctx, "acct-x"); err != nil || len(deleted) != 0 {
			t.Fatalf("deleted %v, err %v; want no credential found and no error", deleted, err)
		}
		if left, set := f.lineageOf(t, "acct-x"); len(left) != 0 || set {
			t.Errorf("after erasure: %d lineage records name the account, owner set %v; want none", len(left), set)
		}
		if !f.lineageExists(otherHash) {
			t.Error("another account's lineage record was deleted")
		}
	})
}

// A chain is read from this account's records but never deletes a lineage record naming another account,
// on erasure or on revoke.
func TestLineageDelete_SparesAnotherAccountsAncestor(t *testing.T) {
	for _, op := range []string{"erase", "revoke"} {
		t.Run(op, func(t *testing.T) {
			ctx := context.Background()
			f := newLineageRecFixture(t)
			foreignHash, foreignSub := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-y", SelfService: true})
			rec := APIKeyRecord{
				KeyID: "kid_forged", Identifier: "acct-x", PermissionsAll: true, ParentKeyID: foreignSub.KeyID,
				AncestorKeyIDs: []string{foreignSub.KeyID}, AncestorHashes: []string{foreignHash},
			}
			body, _ := json.Marshal(rec)
			if err := f.store.writeRecord(ctx, hashAPIKey("sip_forged"), rec, body, MirroredKeyIdleTTL); err != nil {
				t.Fatal(err)
			}
			var err error
			if op == "erase" {
				_, err = f.store.DeleteKeysForIdentifier(ctx, "acct-x")
			} else {
				_, err = f.store.RevokeKeyTree(ctx, "acct-x", foreignSub.KeyID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if f.mr.Exists(cachekeys.APIKey(hashAPIKey("sip_forged")).String()) {
				t.Fatal("setup: the acct-x record was not removed")
			}
			if !f.lineageExists(foreignHash) {
				t.Error("acct-y's lineage record was deleted by an acct-x " + op)
			}
		})
	}
}

// Revoking a key whose credential idled out, by key ID, deletes its lineage record and those of idled-out
// keys between it and its live descendants.
func TestRevoke_IdledOutRootDeletesChainLineage(t *testing.T) {
	ctx := context.Background()
	f := newLineageRecFixture(t)
	rootHash, rootSub := f.mint(t, CreateAPIKeyRequest{Identifier: "acct-x", SelfService: true})
	midHash, midSub := f.mint(t, ChildKeyRequest(rootSub, "mid", nil))
	_, leafPlain, err := f.store.Create(ctx, ChildKeyRequest(midSub, "leaf", nil))
	if err != nil {
		t.Fatal(err)
	}
	f.useDaily(t, leafPlain, 100)
	if f.mr.Exists(cachekeys.APIKey(rootHash).String()) || f.mr.Exists(cachekeys.APIKey(midHash).String()) {
		t.Fatal("setup: want root and mid credentials idled out")
	}
	if err := f.store.RevokeKeyByID(ctx, "acct-x", rootSub.KeyID); err != nil {
		t.Fatal(err)
	}
	if left, _ := f.lineageOf(t, "acct-x"); len(left) != 0 {
		t.Errorf("after revoking the idled-out root: %d lineage records of its tree remain, want 0", len(left))
	}
}
