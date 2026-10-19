package clickhouse

// AssetEntryChangeCursor is the keyset position of one asset entry change: its sort key after the asset.
type AssetEntryChangeCursor struct {
	Ledger      uint32
	TxHash      string
	OpIndex     int32
	ChangeIndex uint32
	Role        string
}

// IsSet reports whether this is a continuation page.
func (c AssetEntryChangeCursor) IsSet() bool { return c.Ledger > 0 }
