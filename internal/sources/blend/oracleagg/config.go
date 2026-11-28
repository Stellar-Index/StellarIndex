package oracleagg

import (
	"errors"
	"fmt"
	"sort"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// ErrConfig marks an adapter configuration that does not decode to the audited layout.
var ErrConfig = errors.New("oracleagg: adapter config")

// Asset is a SEP-40 Asset: Stellar(Address) when Address is set, else Other(Symbol).
type Asset struct {
	Address string
	Symbol  string
}

// Stellar is the Asset::Stellar(address) a pool passes for a reserve.
func Stellar(address string) Asset { return Asset{Address: address} }

func (a Asset) String() string {
	if a.Address != "" {
		return "Stellar(" + a.Address + ")"
	}
	return "Other(" + a.Symbol + ")"
}

// AssetRule is how an adapter prices one asset: the underlying feed and asset, the feed's decimals and
// resolution as the adapter cached them, and the v2 max_dev or the v1 Blocked flag.
type AssetRule struct {
	Feed              string
	FeedAsset         Asset
	FeedDecimals      uint32
	ResolutionSeconds uint32
	MaxDev            uint32 // v2 only, percent; outside 1..99 skips the deviation check
	Blocked           bool   // v1 only; an absent Blocked(asset) entry reads false
}

// Config is an adapter's decoded configuration.
type Config struct {
	Program    Program
	Base       Asset
	BaseAssets []Asset // v2: assets priced at exactly one Base
	Decimals   uint32
	MaxAge     uint64 // v2 only, seconds
	Assets     map[Asset]AssetRule
	// Unreadable holds the configured assets whose own entry or struct did not decode; only they fail.
	Unreadable map[Asset]error
}

// SortedAssets returns the configured assets in a stable order.
func (c Config) SortedAssets() []Asset {
	out := make([]Asset, 0, len(c.Assets))
	for a := range c.Assets {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

var (
	v2InstanceKeys = []string{"Admin", "Assets", "Base", "BaseAssets", "Decimals", "MaxAge", "Oracles"}
	v1InstanceKeys = []string{"Admin", "IsInit"}
)

// DecodeV2Instance decodes a v2.0.0 adapter's instance storage (storage.rs: every key is an instance Symbol).
func DecodeV2Instance(storage []scval.ScMapEntry) (Config, error) {
	f, err := symbolFields(storage, v2InstanceKeys, "Admin", "Base", "Decimals", "MaxAge")
	if err != nil {
		return Config{}, err
	}
	cfg := Config{Program: ProgramV2, Assets: map[Asset]AssetRule{}}
	if cfg.Base, err = DecodeAsset(f["Base"]); err != nil {
		return Config{}, fmt.Errorf("%w: Base: %w", ErrConfig, err)
	}
	if cfg.Decimals, err = scval.AsU32(f["Decimals"]); err != nil {
		return Config{}, fmt.Errorf("%w: Decimals: %w", ErrConfig, err)
	}
	if cfg.MaxAge, err = scval.AsU64(f["MaxAge"]); err != nil {
		return Config{}, fmt.Errorf("%w: MaxAge: %w", ErrConfig, err)
	}
	if v, ok := f["BaseAssets"]; ok {
		if cfg.BaseAssets, err = decodeAssetVec(v); err != nil {
			return Config{}, fmt.Errorf("%w: BaseAssets: %w", ErrConfig, err)
		}
	}
	var oracles []oracleV2
	if v, ok := f["Oracles"]; ok {
		if oracles, err = decodeOraclesV2(v); err != nil {
			return Config{}, err
		}
	}
	if v, ok := f["Assets"]; ok {
		if err := decodeAssetsV2(v, oracles, &cfg); err != nil {
			return Config{}, err
		}
	}
	return cfg, nil
}

// DecodeYBForkV2Instance decodes a YieldBlox-fork adapter's instance storage: its storage.rs and types.rs
// contracttypes are v2.0.0's byte for byte, so only the program differs.
func DecodeYBForkV2Instance(storage []scval.ScMapEntry) (Config, error) {
	cfg, err := DecodeV2Instance(storage)
	if err != nil {
		return Config{}, err
	}
	cfg.Program = ProgramYBForkV2
	return cfg, nil
}

type oracleV2 struct {
	address              string
	decimals, resolution uint32
}

// decodeOraclesV2 decodes Vec<OracleConfig{address, decimals, index, resolution}>; index must equal the position,
// since lastprice looks the oracle up with oracles.get(config.oracle_index).
func decodeOraclesV2(v scval.ScVal) ([]oracleV2, error) {
	vec, err := scval.AsVec(v)
	if err != nil {
		return nil, fmt.Errorf("%w: Oracles: %w", ErrConfig, err)
	}
	out := make([]oracleV2, 0, len(vec))
	for i, ov := range vec {
		m, err := structFields(ov, "address", "decimals", "index", "resolution")
		if err != nil {
			return nil, fmt.Errorf("%w: Oracles[%d]: %w", ErrConfig, i, err)
		}
		var o oracleV2
		var index uint32
		if o.address, err = scval.AsAddressStrkey(m["address"]); err == nil {
			if o.decimals, err = scval.AsU32(m["decimals"]); err == nil {
				if o.resolution, err = scval.AsU32(m["resolution"]); err == nil {
					index, err = scval.AsU32(m["index"])
				}
			}
		}
		if err != nil {
			return nil, fmt.Errorf("%w: Oracles[%d]: %w", ErrConfig, i, err)
		}
		if int(index) != i {
			return nil, fmt.Errorf("%w: Oracles[%d] carries index %d", ErrConfig, i, index)
		}
		out = append(out, o)
	}
	return out, nil
}

// decodeAssetsV2 decodes Map<Asset, AssetConfig{asset, max_dev, oracle_index}> into cfg; an entry whose key decodes
// but whose AssetConfig does not is recorded in cfg.Unreadable, failing that asset only.
func decodeAssetsV2(v scval.ScVal, oracles []oracleV2, cfg *Config) error {
	entries, err := scval.AsMap(v)
	if err != nil {
		return fmt.Errorf("%w: Assets: %w", ErrConfig, err)
	}
	for _, e := range entries {
		asset, err := DecodeAsset(e.Key)
		if err != nil {
			return fmt.Errorf("%w: Assets key: %w", ErrConfig, err)
		}
		rule, err := decodeAssetV2(e.Val, oracles)
		if err != nil {
			cfg.markUnreadable(asset, fmt.Errorf("%w: Assets[%s]: %w", ErrConfig, asset, err))
			continue
		}
		cfg.Assets[asset] = rule
	}
	return nil
}

func decodeAssetV2(v scval.ScVal, oracles []oracleV2) (AssetRule, error) {
	m, err := structFields(v, "asset", "max_dev", "oracle_index")
	if err != nil {
		return AssetRule{}, err
	}
	var rule AssetRule
	var idx uint32
	if rule.FeedAsset, err = DecodeAsset(m["asset"]); err == nil {
		if rule.MaxDev, err = scval.AsU32(m["max_dev"]); err == nil {
			idx, err = scval.AsU32(m["oracle_index"])
		}
	}
	if err != nil {
		return AssetRule{}, err
	}
	if int(idx) >= len(oracles) {
		return AssetRule{}, fmt.Errorf("oracle_index %d outside %d oracles", idx, len(oracles))
	}
	o := oracles[idx]
	rule.Feed, rule.FeedDecimals, rule.ResolutionSeconds = o.address, o.decimals, o.resolution
	return rule, nil
}

// IsBase reports whether a v2 adapter prices the asset as exactly one Base without reading a feed.
func (c Config) IsBase(a Asset) bool {
	return c.Program.v2Storage() && (a == c.Base || containsAsset(c.BaseAssets, a))
}

func (c *Config) markUnreadable(a Asset, err error) {
	if c.Unreadable == nil {
		c.Unreadable = map[Asset]error{}
	}
	c.Unreadable[a] = err
}

// EntryLookup reads one persistent entry of the adapter by its ScVal key; ok=false when the lake holds no such
// entry. An archived or undecodable entry is an error, never absent.
type EntryLookup func(key scval.ScVal) (val scval.ScVal, ok bool, err error)

// DecodeV1 decodes a v1.0.0 adapter: Admin and IsInit in instance storage; Base, Assets, Decimals and one
// AssetConfig(asset) per asset in persistent storage, plus Blocked(asset) once block/unblock ran (storage.rs).
func DecodeV1(instance []scval.ScMapEntry, lookup EntryLookup) (Config, error) {
	f, err := symbolFields(instance, v1InstanceKeys, "Admin", "IsInit")
	if err != nil {
		return Config{}, err
	}
	if init, err := scval.AsBool(f["IsInit"]); err != nil || !init {
		return Config{}, fmt.Errorf("%w: IsInit is not true", ErrConfig)
	}
	cfg := Config{Program: ProgramV1, Assets: map[Asset]AssetRule{}}
	base, err := mustLookup(lookup, symbolKey("Base"))
	if err != nil {
		return Config{}, err
	}
	if cfg.Base, err = DecodeAsset(base); err != nil {
		return Config{}, fmt.Errorf("%w: Base: %w", ErrConfig, err)
	}
	dec, err := mustLookup(lookup, symbolKey("Decimals"))
	if err != nil {
		return Config{}, err
	}
	if cfg.Decimals, err = scval.AsU32(dec); err != nil {
		return Config{}, fmt.Errorf("%w: Decimals: %w", ErrConfig, err)
	}
	assetsV, err := mustLookup(lookup, symbolKey("Assets"))
	if err != nil {
		return Config{}, err
	}
	assets, err := decodeAssetVec(assetsV)
	if err != nil {
		return Config{}, fmt.Errorf("%w: Assets: %w", ErrConfig, err)
	}
	for _, a := range assets {
		rule, err := decodeV1Asset(lookup, a)
		if err != nil {
			cfg.markUnreadable(a, err)
			continue
		}
		cfg.Assets[a] = rule
	}
	return cfg, nil
}

func decodeV1Asset(lookup EntryLookup, a Asset) (AssetRule, error) {
	ak, err := AssetScVal(a)
	if err != nil {
		return AssetRule{}, err
	}
	cv, err := mustLookup(lookup, variantKey("AssetConfig", ak))
	if err != nil {
		return AssetRule{}, err
	}
	m, err := structFields(cv, "asset", "decimals", "oracle_id", "resolution")
	if err != nil {
		return AssetRule{}, fmt.Errorf("%w: AssetConfig(%s): %w", ErrConfig, a, err)
	}
	var rule AssetRule
	if rule.FeedAsset, err = DecodeAsset(m["asset"]); err == nil {
		if rule.FeedDecimals, err = scval.AsU32(m["decimals"]); err == nil {
			if rule.Feed, err = scval.AsAddressStrkey(m["oracle_id"]); err == nil {
				rule.ResolutionSeconds, err = scval.AsU32(m["resolution"])
			}
		}
	}
	if err != nil {
		return AssetRule{}, fmt.Errorf("%w: AssetConfig(%s): %w", ErrConfig, a, err)
	}
	bv, ok, err := lookup(variantKey("Blocked", ak))
	if err != nil {
		return AssetRule{}, fmt.Errorf("Blocked(%s): %w", a, err)
	}
	if ok {
		if rule.Blocked, err = scval.AsBool(bv); err != nil {
			return AssetRule{}, fmt.Errorf("%w: Blocked(%s): %w", ErrConfig, a, err)
		}
	}
	return rule, nil
}

func mustLookup(lookup EntryLookup, key scval.ScVal) (scval.ScVal, error) {
	v, ok, err := lookup(key)
	if err != nil {
		// Not ErrConfig: a failed read is ours, not a layout we cannot decode.
		return scval.ScVal{}, fmt.Errorf("%s: %w", scval.Display(key), err)
	}
	if !ok {
		return scval.ScVal{}, fmt.Errorf("%w: %s: entry missing", ErrConfig, scval.Display(key))
	}
	return v, nil
}

// DecodeAsset decodes the contracttype enum Asset: Vec[Symbol("Stellar"), Address] or Vec[Symbol("Other"), Symbol].
func DecodeAsset(v scval.ScVal) (Asset, error) {
	t, err := scval.AsTupleN(v, 2)
	if err != nil {
		return Asset{}, fmt.Errorf("asset: %w", err)
	}
	tag, err := scval.AsSymbol(t[0])
	if err != nil {
		return Asset{}, fmt.Errorf("asset tag: %w", err)
	}
	switch tag {
	case "Stellar":
		addr, err := scval.AsAddressStrkey(t[1])
		if err != nil {
			return Asset{}, fmt.Errorf("asset Stellar: %w", err)
		}
		return Asset{Address: addr}, nil
	case "Other":
		sym, err := scval.AsSymbol(t[1])
		if err != nil {
			return Asset{}, fmt.Errorf("asset Other: %w", err)
		}
		return Asset{Symbol: sym}, nil
	}
	return Asset{}, fmt.Errorf("asset: unknown variant %q", tag)
}

func decodeAssetVec(v scval.ScVal) ([]Asset, error) {
	vec, err := scval.AsVec(v)
	if err != nil {
		return nil, err
	}
	out := make([]Asset, 0, len(vec))
	for _, e := range vec {
		a, err := DecodeAsset(e)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// AssetScVal encodes a as the Asset enum ScVal the contract keys its per-asset entries with.
func AssetScVal(a Asset) (scval.ScVal, error) {
	if a.Address == "" {
		return scval.NewVec(scval.NewSymbol("Other"), scval.NewSymbol(a.Symbol)), nil
	}
	addr, err := scval.NewContractAddress(a.Address)
	if err != nil {
		return scval.ScVal{}, fmt.Errorf("%w: asset %s: %w", ErrConfig, a, err)
	}
	return scval.NewVec(scval.NewSymbol("Stellar"), addr), nil
}

func symbolKey(name string) scval.ScVal { return scval.NewSymbol(name) }

func variantKey(variant string, arg scval.ScVal) scval.ScVal {
	return scval.NewVec(scval.NewSymbol(variant), arg)
}

// symbolFields indexes Symbol-keyed storage by name, rejecting any key outside allowed and requiring each of required.
func symbolFields(entries []scval.ScMapEntry, allowed []string, required ...string) (map[string]scval.ScVal, error) {
	out := make(map[string]scval.ScVal, len(entries))
	for _, e := range entries {
		name, err := scval.AsSymbol(e.Key)
		if err != nil {
			return nil, fmt.Errorf("%w: storage key %s is not a Symbol", ErrConfig, scval.Display(e.Key))
		}
		if !contains(allowed, name) {
			return nil, fmt.Errorf("%w: unknown storage key %q", ErrConfig, name)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("%w: duplicate storage key %q", ErrConfig, name)
		}
		out[name] = e.Val
	}
	for _, r := range required {
		if _, ok := out[r]; !ok {
			return nil, fmt.Errorf("%w: storage key %q missing", ErrConfig, r)
		}
	}
	return out, nil
}

// structFields decodes a contracttype struct (a Symbol-keyed map) that must carry exactly the named fields.
func structFields(v scval.ScVal, names ...string) (map[string]scval.ScVal, error) {
	entries, err := scval.AsMap(v)
	if err != nil {
		return nil, err
	}
	return symbolFields(entries, names, names...)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
