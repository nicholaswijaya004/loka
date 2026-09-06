package lab

import (
	"sync"
	"sync/atomic"
)

type UnsafeCounter struct {
	count int
}

func (c *UnsafeCounter) Increment() {
	c.count++
}

func (c *UnsafeCounter) Value() int {
	return c.count
}

type MutexCounter struct {
	mu    sync.Mutex
	count int
}

func (c *MutexCounter) Increment() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.count++
}

func (c *MutexCounter) Value() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}

type AtomicCounter struct {
	count atomic.Int64
}

func (c *AtomicCounter) Increment() {
	c.count.Add(1)
}

func (c *AtomicCounter) Value() int64 {
	return c.count.Load()
}
