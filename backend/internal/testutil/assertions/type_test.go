package assertions

import (
	"fmt"
	"strings"
	"testing"
)

func TestType(t *testing.T) {
	source := map[string]any{"value": 1}
	got := Type[map[string]any](t, source)
	got["value"] = 2
	if source["value"] != 2 {
		t.Fatal("must preserve map identity")
	}
	var pointer *int
	if Type[*int](t, pointer) != nil {
		t.Fatal("typed nil pointer must be preserved")
	}
	if Type[fmt.Stringer](t, &strings.Builder{}).String() != "" {
		t.Fatal("interface type assertion failed")
	}
}

type failedTypeAssertion struct{}
type failureReporter struct {
	message string
	helpers int
}

func (r *failureReporter) Helper() { r.helpers++ }
func (r *failureReporter) Fatalf(format string, args ...any) {
	r.message = fmt.Sprintf(format, args...)
	panic(failedTypeAssertion{})
}

func TestTypeMismatch(t *testing.T) {
	for _, value := range []any{nil, "not a map"} {
		func() {
			reporter := &failureReporter{}
			defer func() {
				if _, ok := recover().(failedTypeAssertion); !ok {
					t.Fatal("type mismatch must stop the test")
				}
				if reporter.helpers != 1 || !strings.Contains(reporter.message, "expected type map[string]interface {}") {
					t.Fatalf("missing helper attribution or type diagnostic: %+v", reporter)
				}
			}()
			Type[map[string]any](reporter, value)
			t.Fatal("type mismatch unexpectedly returned")
		}()
	}
}
