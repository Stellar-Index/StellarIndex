package v1

import (
	"context"
	"time"
)

// sacNotWrappedTTL bounds how long a "not a SAC" answer is reused; a contract instance can land in the lake after first sight.
const sacNotWrappedTTL = 10 * time.Minute

// sacWrappedAssetID is [Server.resolveSACToClassic] as an asset_id. A success is memoised for good (the address fixes the
// wrapped asset), a definite "not a SAC" for sacNotWrappedTTL; a read error is not cached.
func (s *Server) sacWrappedAssetID(ctx context.Context, contractID string) (string, bool) {
	id, ok, _ := s.sacWrappedAssetIDErr(ctx, contractID)
	return id, ok
}

// sacWrappedAssetIDErr is [Server.sacWrappedAssetID] that also returns the lake read error.
func (s *Server) sacWrappedAssetIDErr(ctx context.Context, contractID string) (string, bool, error) {
	if v, ok := s.sacWrapped.Load(contractID); ok {
		return v.(string), true, nil
	}
	if v, ok := s.sacNotWrapped.Load(contractID); ok {
		if time.Now().Before(v.(time.Time)) {
			return "", false, nil
		}
		s.sacNotWrapped.Delete(contractID)
	}
	asset, ok, err := s.resolveSAC(ctx, contractID)
	if !ok {
		if err == nil {
			s.sacNotWrapped.Store(contractID, time.Now().Add(sacNotWrappedTTL))
		}
		return "", false, err
	}
	id := asset.String()
	s.sacWrapped.Store(contractID, id)
	return id, true, nil
}
