package phoenix

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

var camelBoundary = regexp.MustCompile(`([a-z0-9])([A-Z])`)

// TestREADMEGroupingKeyMatchesDecoder pins the README's stated event-grouping
// tuple to groupKey's fields, so a reader reconstructing the correlation from
// the doc cannot drop a discriminator the decoder depends on (ContractID
// isolates each pool's reassembly within a router multihop).
func TestREADMEGroupingKeyMatchesDecoder(t *testing.T) {
	typ := reflect.TypeOf(groupKey{})
	fields := make([]string, 0, typ.NumField())
	for i := range typ.NumField() {
		name := strings.ReplaceAll(typ.Field(i).Name, "ID", "Id")
		fields = append(fields, strings.ToLower(camelBoundary.ReplaceAllString(name, "${1}_${2}")))
	}
	want := "(" + strings.Join(fields, ", ") + ")"

	raw, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	readme := strings.Join(strings.Fields(string(raw)), " ")
	if !strings.Contains(readme, "group its N events** by `"+want+"`") {
		t.Fatalf("README.md must state the grouping key as `%s` (groupKey's fields, in order); it states a different tuple", want)
	}
}
