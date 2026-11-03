// Package assertions provides checked values for test setup and assertions.
package assertions

import "reflect"

type fatalReporter interface {
	Helper()
	Fatalf(format string, args ...any)
}

// Type requires value to have type T and returns it without copying its contents.
func Type[T any](t fatalReporter, value any) T {
	t.Helper()
	typed, ok := value.(T)
	if !ok {
		t.Fatalf("expected type %v, got %T", reflect.TypeFor[T](), value)
	}
	return typed
}
