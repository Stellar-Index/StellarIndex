// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"reflect"
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
	// One credential refresh per Lookup, plus one slide (the key and its root) per hour.
	if got := lookups(10); got != 10+2 {
		t.Errorf("10 Lookups in one hour sent %d EXPIREs, want 12", got)
	}
	f.advance(lineageSlideInterval)
	if got := lookups(1); got != 1+2 {
		t.Errorf("first Lookup after the interval sent %d EXPIREs, want 3", got)
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
	type outcome struct {
		sub Subject
		err string
		ttl time.Duration
	}
	run := func(t *testing.T, failWith string) map[string]outcome {
		f := newLineageRecFixture(t)
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
		if err := f.store.RevokeKeyByID(context.Background(), "acct-z", rootSub.KeyID); err != nil && failWith != "" && strings.HasPrefix(failWith, "NOPERM") {
			t.Fatalf("revoke with the lineage ACL missing: %v", err)
		}
		if _, err := f.store.DeleteKeysForIdentifier(context.Background(), "acct-x"); err != nil && strings.HasPrefix(failWith, "NOPERM") {
			t.Fatalf("erasure delete with the lineage ACL missing: %v", err)
		}
		if failWith != "" && f.denier.denied.Load() == 0 {
			t.Fatal("no lineage command was refused: the failure path was not exercised")
		}
		return out
	}
	want := run(t, "")
	for _, failWith := range []string{"NOPERM No permissions to access a key", "OOM command not allowed when used memory > 'maxmemory'."} {
		before := testutil.ToFloat64(obs.APIKeyLineageWriteFailuresTotal.WithLabelValues(obs.APIKeyLineageOpSlide))
		got := run(t, failWith)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: auth outcomes differ from lineage working:\n got %+v\nwant %+v", failWith, got, want)
		}
		if after := testutil.ToFloat64(obs.APIKeyLineageWriteFailuresTotal.WithLabelValues(obs.APIKeyLineageOpSlide)); after <= before {
			t.Errorf("%s: slide failures not counted", failWith)
		}
	}
}

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
