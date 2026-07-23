package storeio

import "sync/atomic"

const (
	poolIndexBits = 17
	poolIndexMask = uint64(1<<poolIndexBits) - 1
)

// indexPool is an ABA-resistant lock-free stack over a fixed set of indexes.
// The low 17 bits store index+1 (enough for all 65,536 Device buffers); the
// upper 47 bits are a version incremented by every successful push or pop.
// notify is only a parking hint after the stack is observed empty and is not
// part of the ownership protocol.
type indexPool struct {
	head   atomic.Uint64
	next   []atomic.Uint32
	notify chan struct{}
	waiter atomic.Uint32
}

func newIndexPool(count int) *indexPool {
	p := &indexPool{
		next:   make([]atomic.Uint32, count),
		notify: make(chan struct{}, 1),
	}
	for index := count - 1; index >= 0; index-- {
		p.push(uint32(index))
	}
	return p
}

func (p *indexPool) pop() (uint32, bool) {
	for {
		head := p.head.Load()
		code := head & poolIndexMask
		if code == 0 {
			return 0, false
		}
		index := uint32(code - 1)
		next := uint64(p.next[index].Load())
		updated := ((head >> poolIndexBits) + 1) << poolIndexBits
		updated |= next
		if p.head.CompareAndSwap(head, updated) {
			return index, true
		}
	}
}

func (p *indexPool) push(index uint32) {
	code := uint64(index) + 1
	for {
		head := p.head.Load()
		p.next[index].Store(uint32(head & poolIndexMask))
		updated := ((head >> poolIndexBits) + 1) << poolIndexBits
		updated |= code
		if p.head.CompareAndSwap(head, updated) {
			if head&poolIndexMask == 0 && p.waiter.Load() != 0 {
				select {
				case p.notify <- struct{}{}:
				default:
				}
			}
			return
		}
	}
}
