package lab

import (
	"errors"
	"sync"
	"time"
)

var ErrSoldOut = errors.New("sold out")

// UnsafeInventory double-sells under concurrency.
type UnsafeInventory struct {
	available int
	sold      int
}

func NewUnsafeInventory(seats int) *UnsafeInventory {
	return &UnsafeInventory{available: seats}
}

func (i *UnsafeInventory) Book() error {
	if i.available <= 0 {
		return ErrSoldOut
	}
	time.Sleep(time.Microsecond)
	i.available--
	i.sold++
	return nil
}

func (i *UnsafeInventory) Stats() (available, sold int) {
	return i.available, i.sold
}

type SafeInventory struct {
	mu        sync.Mutex
	available int
	sold      int
}

func NewSafeInventory(seats int) *SafeInventory {
	return &SafeInventory{available: seats}
}

func (i *SafeInventory) Book() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.available <= 0 {
		return ErrSoldOut
	}
	time.Sleep(time.Microsecond)
	i.available--
	i.sold++
	return nil
}

func (i *SafeInventory) Stats() (available, sold int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.available, i.sold
}
