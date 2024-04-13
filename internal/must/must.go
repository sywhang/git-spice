// Package must provides runtime assertions.
// Violation of these assertions indicates a program fault,
// and should cause a crash to prevent operating with invalid data.
package must

import (
	"fmt"
	"strings"
)

func BeEqualf[T comparable](a, b T, format string, args ...any) {
	if a != b {
		panicErrorf("%v\nwant a == b\na = %v\nb = %v",
			fmt.Errorf(format, args...), a, b,
		)
	}
}

func NotBeEqualf[T comparable](a, b T, format string, args ...any) {
	if a == b {
		panicErrorf("%v\nwant a != b\na = %v\nb = %v",
			fmt.Errorf(format, args...), a, b)
	}
}

func NotBeBlankf(s string, format string, args ...any) {
	s = strings.TrimSpace(s)
	if len(s) == 0 {
		panicErrorf(format, args...)
	}
}

func NotBeEmptyf[T any](es []T, format string, args ...any) {
	if len(es) == 0 {
		panicErrorf(format, args...)
	}
}

func panicErrorf(format string, args ...any) {
	panic(fmt.Errorf(format, args...))
}
