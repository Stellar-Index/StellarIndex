package clickhouse

import (
	"context"
	"reflect"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// emptyProbeConn answers every probe with an existing-but-empty object.
type emptyProbeConn struct{ driver.Conn }

func (emptyProbeConn) Query(context.Context, string, ...any) (driver.Rows, error) {
	return &probeRows{consumed: true}, nil
}

// TestNewExplorerReader_EverySchemaProbeIsNamed — an unnamed probe would
// export under probe="" and collide with every other unnamed one.
func TestNewExplorerReader_EverySchemaProbeIsNamed(t *testing.T) {
	v := reflect.ValueOf(newExplorerReader(nil)).Elem()
	probeType := reflect.TypeOf(schemaProbe{})
	seen := map[string]string{}
	for i := range v.NumField() {
		f := v.Type().Field(i)
		if f.Type != probeType {
			continue
		}
		name := v.Field(i).FieldByName("name").String()
		if name == "" {
			t.Errorf("schemaProbe field %s has no name", f.Name)
			continue
		}
		if other, dup := seen[name]; dup {
			t.Errorf("schemaProbe fields %s and %s share name %q", other, f.Name, name)
		}
		seen[name] = f.Name
	}
	if len(seen) == 0 {
		t.Fatal("found no schemaProbe fields — the reflection walk is broken")
	}
}
