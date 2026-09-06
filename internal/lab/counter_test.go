package lab

import (
	"os"
	"sync"
	"testing"
)

const (
	goroutines = 1000
	perRoutine = 1000
)

func TestUnsafeCounter(t *testing.T) {
	if os.Getenv("LAB_RACE") == "" {
		t.Skip("demonstrates a deliberate data race; set LAB_RACE=1 to run")
	}
	c := &UnsafeCounter{}
	var wg sync.WaitGroup

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perRoutine; j++ {
				c.Increment()
			}
		}()
	}
	wg.Wait()
	want := goroutines * perRoutine
	got := c.Value()
	t.Logf("unsafe: want %d, got %d, lost %d (%.2f%%)",
		want, got, want-got, float64(want-got)/float64(want)*100)
}

func TestMutexCounter(t *testing.T) {
	c := &MutexCounter{}
	var wg sync.WaitGroup

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perRoutine; j++ {
				c.Increment()
			}
		}()
	}
	wg.Wait()

	want := goroutines * perRoutine
	if got := c.Value(); got != want {
		t.Errorf("mutex: want %d, got %d", want, got)
	}
}

func TestAtomicCounter(t *testing.T) {
	c := &AtomicCounter{}
	var wg sync.WaitGroup

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perRoutine; j++ {
				c.Increment()
			}
		}()
	}
	wg.Wait()

	want := int64(goroutines * perRoutine)
	if got := c.Value(); got != want {
		t.Errorf("atomic: want %d, got %d", want, got)
	}
}

func BenchmarkMutexCounter(b *testing.B) {
	c := &MutexCounter{}
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			c.Increment()
		}
	})
}

func BenchmarkAtomicCounter(b *testing.B) {
	c := &AtomicCounter{}
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			c.Increment()
		}
	})
}
