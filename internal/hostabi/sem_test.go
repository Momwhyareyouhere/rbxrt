package hostabi

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// TestSemaphore passes 2000 posts through a semaphore to two waiting threads
// and checks a waiter really sleeps (no CPU spin) while the count is 0.
func TestSemaphore(t *testing.T) {
	var sem [4]uint32
	p := uintptr(unsafe.Pointer(&sem[0]))
	goSemInit(p, 0, 0)

	const n = 2000
	var got, exited sync.WaitGroup
	got.Add(n)
	exited.Add(2)
	var stop atomic.Bool
	for i := 0; i < 2; i++ {
		go func() {
			runtime.LockOSThread()
			defer exited.Done()
			for {
				goSemWait(p)
				if stop.Load() {
					return
				}
				got.Done()
			}
		}()
	}
	// Let both waiters block, then make sure they are asleep in the kernel.
	time.Sleep(50 * time.Millisecond)
	if w := sem[1]; w != 2 {
		t.Fatalf("%d waiters asleep, want 2", w)
	}
	for i := 0; i < n; i++ {
		goSemPost(p)
	}
	done := make(chan struct{})
	go func() { got.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("waiters stuck: count %d, waiters %d", sem[0], sem[1])
	}
	// Release the two waiters so they do not stay asleep in the kernel.
	stop.Store(true)
	goSemPost(p)
	goSemPost(p)
	exited.Wait()
}
