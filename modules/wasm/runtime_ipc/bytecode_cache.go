package wasm_runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strconv"
	"sync"

	"vsc-node/lib/lru"
	"vsc-node/lib/vsclog"

	"github.com/second-state/WasmEdge-go/wasmedge"
)

var log = vsclog.Module("wasm")

// Contract bytecode is immutable (addressable by CID), yet every execution
// previously re-parsed and re-validated the raw wasm bytes into a wasmedge
// AST module. Parsed ASTs are cached here keyed by a hash of the bytecode so
// repeated calls to the same contract skip the load/parse phase. The wasmedge
// C API (WasmEdge_VMRegisterModuleFromASTModule) is documented thread-safe, so
// one AST can be shared across the per-execution VMs; instantiation still
// happens per VM (imports and gas accounting are execution-local).
//
// The cache holds C-backed AST handles that must be explicitly released, so
// evicted entries are handed to cachedBytecode.drop(), which defers the
// release until no execution is using the entry anymore.
const bytecodeCacheDefaultCapacity = 128

// bytecodeCacheEnv overrides the cache capacity (must be a positive integer).
const bytecodeCacheEnv = "VSC_WASM_BYTECODE_CACHE_SIZE"

// bytecodeCacheCapacity is read once at init; a misconfigured value falls back
// to the default rather than panicking at startup.
var bytecodeCacheCapacity = bytecodeCacheCapacityFromEnv()

func bytecodeCacheCapacityFromEnv() int {
	if raw := os.Getenv(bytecodeCacheEnv); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
		log.Warn("invalid "+bytecodeCacheEnv, "value", raw, "fallback", bytecodeCacheDefaultCapacity)
	}
	return bytecodeCacheDefaultCapacity
}

var bytecodeCache = lru.NewEvict[string, *cachedBytecode](bytecodeCacheCapacity, func(_ string, bc *cachedBytecode) {
	bc.drop()
})

// bytecodeLoadMu serializes cache-miss parses so the same key is never parsed
// twice concurrently (loads are rare — one per contract per cache lifetime).
var bytecodeLoadMu sync.Mutex

// cachedBytecode wraps a parsed wasmedge AST with a refcount so eviction can
// never free the C resource while another goroutine is registering it into its
// VM. acquire()/release() must be paired; drop() is only called by the cache.
type cachedBytecode struct {
	mu   sync.Mutex
	refs int
	dead bool
	ast  *wasmedge.AST
}

// acquire increments the refcount and returns the AST, or nil if the entry
// has been evicted (dropped) since it was fetched from the cache. A non-nil
// return must be matched with exactly one release().
func (b *cachedBytecode) acquire() *wasmedge.AST {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.dead {
		return nil
	}
	b.refs++
	return b.ast
}

// release decrements the refcount. If the entry was dropped (evicted) while
// still in use, the AST is released once the last user is done with it.
func (b *cachedBytecode) release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refs--
	if b.refs == 0 && b.dead && b.ast != nil {
		b.ast.Release()
		b.ast = nil
	}
}

// drop marks the entry dead, releasing the AST now if it is unused, or
// deferring to the last release() otherwise. Safe to call more than once.
func (b *cachedBytecode) drop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.dead = true
	if b.refs == 0 && b.ast != nil {
		b.ast.Release()
		b.ast = nil
	}
}

// isDead reports whether the entry has been dropped (evicted).
func (b *cachedBytecode) isDead() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dead
}

// getOrLoadBytecode returns the cached parsed bytecode for code, parsing and
// caching it on a miss. Dead (evicted) entries are transparently reloaded, so
// a returned entry may be safely acquired immediately. Only successful parses
// are cached; invalid bytecode always takes the error path.
func getOrLoadBytecode(code []byte) (*cachedBytecode, error) {
	sum := sha256.Sum256(code)
	key := hex.EncodeToString(sum[:])

	if bc, ok := bytecodeCache.Get(key); ok && !bc.isDead() {
		return bc, nil
	}

	bytecodeLoadMu.Lock()
	defer bytecodeLoadMu.Unlock()

	// Double-check: another goroutine may have loaded this key while we waited.
	if bc, ok := bytecodeCache.Get(key); ok && !bc.isDead() {
		return bc, nil
	}

	ast, err := loadBytecode(code)
	if err != nil {
		return nil, err
	}
	bc := &cachedBytecode{ast: ast}
	bytecodeCache.Put(key, bc)
	return bc, nil
}

// loadBytecode parses (but does not instantiate) raw wasm bytes into a
// wasmedge AST module. The loader is created per load because it is not
// thread-safe; parsing is serialized by bytecodeLoadMu anyway.
func loadBytecode(code []byte) (*wasmedge.AST, error) {
	loader := wasmedge.NewLoader()
	defer loader.Release()
	return loader.LoadBuffer(code)
}
