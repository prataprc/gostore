package llrb

//#include <stdlib.h>
import "C"

import "unsafe"
import "encoding/binary"

// mempool manages a memory block sliced up into equal sized chunks.
type mempool struct {
	capacity int64          // memory managed by this pool
	size     int            // fixed size blocks in this pool
	base     unsafe.Pointer // pool's base pointer
	freelist []byte         // free block book-keeping
	freeoff  int
}

func newmempool(size, n int) *mempool {
	if (n & 0x7) != 0 { // n must be a multiple of 8
		n = ((n >> 3) + 1) << 3
	}
	capacity := int64(size * n)
	base := C.malloc(C.size_t(capacity))
	freelist := make([]byte, n/8)
	for i := range freelist {
		freelist[i] = 0xff // every block is free to begin with.
	}
	pool := &mempool{
		capacity: capacity,
		size:     size,
		base:     base,
		freelist: freelist,
		freeoff:  0,
	}
	return pool
}

func (pool *mempool) alloc() (unsafe.Pointer, bool) {
	var safeoff int

	if pool.freeoff < 0 {
		return nil, false
	}
	byt := pool.freelist[pool.freeoff]
	if byt == 0 {
		panic("mempool.alloc(): invalid free-offset")
	}
	sz, k := pool.size, findfirstset8(byt)
	ptr := uintptr(pool.base) + uintptr(((pool.freeoff*8)*sz)+(int(k)*sz))
	pool.freelist[pool.freeoff] = clearbit8(byt, uint8(k))
	safeoff, pool.freeoff = pool.freeoff, -1
	for i := safeoff; i < len(pool.freelist); i++ {
		if pool.freelist[i] > 0 {
			pool.freeoff = i
			break
		}
	}
	return unsafe.Pointer(ptr), true
}

func (pool *mempool) free(ptr unsafe.Pointer) {
	nthblock := uint64(uintptr(ptr)-uintptr(pool.base)) / uint64(pool.size)
	nthoff := (nthblock / 8)
	pool.freelist[nthoff] = setbit8(pool.freelist[nthoff], uint8(nthblock%8))
	if int(nthoff) < pool.freeoff {
		pool.freeoff = int(nthoff)
	}
}

func (pool *mempool) release() {
	C.free(pool.base)
}

// compare whether pool's base ptr is less than other pool's base ptr.
func (pool *mempool) less(other *mempool) bool {
	return uintptr(pool.base) < uintptr(other.base)
}

//---- local functions

func (pool *mempool) allocated() int64 {
	blocks := 0
	q, r := len(pool.freelist)/4, len(pool.freelist)%4
	for i := 1; i <= q; i++ {
		v, _ := binary.Uvarint(pool.freelist[(i-1)*4 : i*4])
		blocks += int(zerosin32(uint32(v & 0xffffffff)))
		blocks += int(zerosin32(uint32(v >> 0xffffffff)))
	}
	for i := q * 4; i < r; i++ {
		blocks += int(zerosin8(pool.freelist[i]))
	}
	return int64(blocks * pool.size)
}

func (pool *mempool) available() int64 {
	return pool.capacity - pool.allocated()
}

// mempools sortable based on base-pointer.
type mempools []*mempool

func (pools mempools) Len() int {
	return len(pools)
}

func (pools mempools) Less(i, j int) bool {
	return pools[i].less(pools[j])
}

func (pools mempools) Swap(i, j int) {
	pools[i], pools[j] = pools[j], pools[i]
}
