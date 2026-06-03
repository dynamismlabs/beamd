package beamdapi

import (
	"reflect"
	"sort"
	"strings"
)

// JSONFields returns the sorted top-level JSON field names of a struct value or
// pointer, with `,omitempty` stripped and `-` skipped. Pointer- and value-typed
// fields compare equal, so a hand-written struct and the pointer-heavy generated
// type expose the same field set. Used by per-package conformance tests to guard
// the CLI's hand-written wire structs against drift from the shared OpenAPI spec.
func JSONFields(v any) []string {
	t := reflect.TypeOf(v)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	out := []string{}
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
