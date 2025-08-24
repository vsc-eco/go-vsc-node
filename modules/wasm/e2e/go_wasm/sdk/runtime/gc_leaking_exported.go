// https://github.com/tinygo-org/tinygo/blob/2a76ceb7dd5ea5a834ec470b724882564d9681b3/src/runtime/arch_tinygowasm.go#L42
// https://github.com/tinygo-org/tinygo/blob/2a76ceb7dd5ea5a834ec470b724882564d9681b3/src/runtime/gc_leaking.go

//go:build gc.custom

package runtime

// This GC implementation is the simplest useful memory allocator possible: it
// only allocates memory and never frees it. For some constrained systems, it
// may be the only memory allocator possible.

import (
	realRuntime "runtime"
	"unsafe"
)

// https://github.com/tinygo-org/tinygo/blob/2a76ceb7dd5ea5a834ec470b724882564d9681b3/src/runtime/arch_tinygowasm.go#L19
//
//go:extern __heap_base
var heapStartSymbol [0]byte

var heapStart = uintptr(unsafe.Pointer(&heapStartSymbol))

// https://github.com/tinygo-org/tinygo/blob/2a76ceb7dd5ea5a834ec470b724882564d9681b3/src/runtime/arch_tinygowasm.go#L34
//
//export llvm.wasm.memory.size.i32
func wasm_memory_size(index int32) int32

// https://github.com/tinygo-org/tinygo/blob/2a76ceb7dd5ea5a834ec470b724882564d9681b3/src/runtime/arch_tinygowasm.go#L34
const wasmPageSize = 64 * 1024

var heapEnd = uintptr(wasm_memory_size(0) * wasmPageSize)

// Ever-incrementing pointer: no memory is freed.
var heapptr = heapStart

// Total amount allocated for runtime.MemStats
var gcTotalAlloc uint64

// Total number of calls to alloc()
var gcMallocs uint64

// Total number of objected freed; for leaking collector this stays 0
const gcFrees = 0

// trap is a compiler hint that this function cannot be executed. It is
// translated into either a trap instruction or a call to abort().
//
//export llvm.trap
func trap()

//go:wasmexport alloc
func Alloc(size uintptr) unsafe.Pointer {
	return alloc(size, unsafe.Pointer(nil))
}

// Inlining alloc() speeds things up slightly but bloats the executable by 50%,
// see https://github.com/tinygo-org/tinygo/issues/2674.  So don't.
//
//go:noinline
//go:linkname alloc runtime.alloc
func alloc(size uintptr, layout unsafe.Pointer) unsafe.Pointer {
	// TODO: this can be optimized by not casting between pointers and ints so
	// much. And by using platform-native data types (e.g. *uint8 for 8-bit
	// systems).
	size = align(size)
	addr := heapptr
	gcTotalAlloc += uint64(size)
	gcMallocs++
	heapptr += size
	for heapptr >= heapEnd {
		// Try to increase the heap and check again.
		if growHeap() {
			continue
		}
		// Failed to make the heap bigger, so we must really be out of memory.
		trap() // NOTE: alterred from original impl since we just trap. `runtimePanic("out of memory")`
		// see: https://github.com/tinygo-org/tinygo/blob/2a76ceb7dd5ea5a834ec470b724882564d9681b3/src/runtime/panic.go#L72
	}
	pointer := unsafe.Pointer(addr)
	zero_new_alloc(pointer, size)
	return pointer
}

//go:inline
func zero_new_alloc(ptr unsafe.Pointer, size uintptr) {
	memzero(ptr, size)
}

// Copy size bytes from src to dst. The memory areas must not overlap.
// This function is implemented by the compiler as a call to a LLVM intrinsic
// like llvm.memcpy.p0.p0.i32(dst, src, size, false).
//
//go:linkname memcpy runtime.memcpy
func memcpy(dst, src unsafe.Pointer, size uintptr)

// Copy size bytes from src to dst. The memory areas may overlap and will do the
// correct thing.
// This function is implemented by the compiler as a call to a LLVM intrinsic
// like llvm.memmove.p0.p0.i32(dst, src, size, false).
//
//go:linkname memmove runtime.memmove
func memmove(dst, src unsafe.Pointer, size uintptr)

// Set the given number of bytes to zero.
// This function is implemented by the compiler as a call to a LLVM intrinsic
// like llvm.memset.p0.i32(ptr, 0, size, false).
//
//go:linkname memzero runtime.memzero
func memzero(ptr unsafe.Pointer, size uintptr)

// This can be exported to wasm if needed like this: //go:wasmexport realloc
//
//go:linkname realloc runtime.realloc
func realloc(ptr unsafe.Pointer, size uintptr) unsafe.Pointer {
	newAlloc := alloc(size, nil)
	if ptr == nil {
		return newAlloc
	}
	// according to POSIX everything beyond the previous pointer's
	// size will have indeterminate values so we can just copy garbage
	memcpy(newAlloc, ptr, size)

	return newAlloc
}

//go:linkname free runtime.free
func free(ptr unsafe.Pointer) {
	// Memory is never freed.
}

// ReadMemStats populates m with memory statistics.
//
// The returned memory statistics are up to date as of the
// call to ReadMemStats. This would not do GC implicitly for you.
//
//go:linkname ReadMemStats runtime.ReadMemStats
func ReadMemStats(m *realRuntime.MemStats) {
	m.HeapIdle = 0
	m.HeapInuse = gcTotalAlloc
	m.HeapReleased = 0 // always 0, we don't currently release memory back to the OS.

	m.HeapSys = m.HeapInuse + m.HeapIdle
	m.GCSys = 0
	m.TotalAlloc = gcTotalAlloc
	m.Mallocs = gcMallocs
	m.Frees = gcFrees
	m.Sys = uint64(heapEnd - heapStart)
	// no free -- current in use heap is the total allocated
	m.HeapAlloc = gcTotalAlloc
	m.Alloc = m.HeapAlloc
}

//go:linkname GC runtime.GC
func GC() {
	// No-op.
}

func SetFinalizer(obj interface{}, finalizer interface{}) {
	// No-op.
}

//go:linkname initHeap runtime.initHeap
func initHeap() {
	// preinit() may have moved heapStart; reset heapptr
	heapptr = heapStart
}

// setHeapEnd sets a new (larger) heapEnd pointer.
//
//go:linkname setHeapEnd runtime.setHeapEnd
// func setHeapEnd(newHeapEnd uintptr) {
// 	// This "heap" is so simple that simply assigning a new value is good
// 	// enough.
// 	heapEnd = newHeapEnd
// }

const wasmMemoryIndex = 0

//export llvm.wasm.memory.grow.i32
func wasm_memory_grow(index int32, delta int32) int32

func growHeap() bool {
	// Grow memory by the available size, which means the heap size is doubled.
	memorySize := wasm_memory_size(wasmMemoryIndex)
	result := wasm_memory_grow(wasmMemoryIndex, memorySize)
	if result == -1 {
		// Grow failed.
		return false
	}

	heapEnd = uintptr(wasm_memory_size(wasmMemoryIndex) * wasmPageSize)

	// Heap has grown successfully.
	return true
}

func align(ptr uintptr) uintptr {
	// Align to 16, which is the alignment of max_align_t:
	// https://godbolt.org/z/dYqTsWrGq
	const heapAlign = 16
	return (ptr + heapAlign - 1) &^ (heapAlign - 1)
}
