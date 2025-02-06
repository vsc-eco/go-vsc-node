package weak

var alwaysFalse bool
var escapeSink any

// Escape forces any pointers in x to escape to the heap.
func Escape[T any](x T) T {
	if alwaysFalse {
		escapeSink = x
	}
	return x
}
