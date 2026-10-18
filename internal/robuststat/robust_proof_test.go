package robuststat

import "testing"

// TestRobustCentreScale_EmptyValsDoesNotPanic pins that CentreScale on empty vals returns nil, nil instead of panicking in MAD on Median's nil; the guard belongs on the shared primitive.
func TestRobustCentreScale_EmptyValsDoesNotPanic(t *testing.T) {
	centre, scale := CentreScale(nil)
	if centre != nil || scale != nil {
		t.Fatalf("CentreScale(nil) = (%v, %v), want (nil, nil)", centre, scale)
	}
}
