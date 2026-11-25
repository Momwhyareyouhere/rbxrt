package hostabi

import (
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// Guest threads are goroutines, so these tests drive the mutex functions from
// goroutines the way the engine's threads do.

func TestRecursiveMutexStaticInitializer(t *testing.T) {
	m, unmap := pin(t)
	defer unmap()
	*(*uint32)(unsafe.Pointer(m)) = 0x4000 // PTHREAD_RECURSIVE_MUTEX_INITIALIZER_NP
	if r := goPthreadMutexLock(m); r != 0 {
		t.Fatalf("lock: %d", r)
	}
	if r := goPthreadMutexLock(m); r != 0 {
		t.Fatalf("recursive lock: %d", r)
	}
	if r := goPthreadMutexTrylock(m); r != 0 {
		t.Fatalf("recursive trylock: %d", r)
	}
	other := make(chan int)
	go func() { other <- goPthreadMutexTrylock(m) }()
	if r := <-other; r != errEBUSY {
		t.Fatalf("trylock from another thread while held: %d, want EBUSY", r)
	}
	go func() { other <- goPthreadMutexUnlock(m) }()
	if r := <-other; r != errEPERM {
		t.Fatalf("unlock by non-owner: %d, want EPERM", r)
	}
	for i := 0; i < 3; i++ {
		if r := goPthreadMutexUnlock(m); r != 0 {
			t.Fatalf("unlock %d: %d", i, r)
		}
	}
	go func() {
		r := goPthreadMutexTrylock(m)
		if r == 0 {
			goPthreadMutexUnlock(m)
		}
		other <- r
	}()
	if r := <-other; r != 0 {
		t.Fatalf("trylock after full release: %d", r)
	}
}

func TestMutexAttrTypes(t *testing.T) {
	m, unmapM := pin(t)
	defer unmapM()
	a, unmapA := pin(t)
	defer unmapA()
	goPthreadMutexattrInit(a)
	if r := goPthreadMutexattrSettype(a, mutexErrorcheck); r != 0 {
		t.Fatalf("settype: %d", r)
	}
	goPthreadMutexInit(m, a)
	goPthreadMutexLock(m)
	if r := goPthreadMutexLock(m); r != errEDEADLK {
		t.Fatalf("errorcheck relock: %d, want EDEADLK", r)
	}
	goPthreadMutexUnlock(m)

	goPthreadMutexattrSettype(a, mutexRecursive)
	goPthreadMutexInit(m, a)
	goPthreadMutexLock(m)
	if r := goPthreadMutexLock(m); r != 0 {
		t.Fatalf("recursive relock via attr: %d", r)
	}
	goPthreadMutexUnlock(m)
	goPthreadMutexUnlock(m)
}

func TestMutexContention(t *testing.T) {
	m, unmap := pin(t)
	defer unmap()
	goPthreadMutexInit(m, 0)
	counter := 0
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20000; i++ {
				goPthreadMutexLock(m)
				counter++
				goPthreadMutexUnlock(m)
			}
		}()
	}
	wg.Wait()
	if counter != 8*20000 {
		t.Fatalf("counter = %d, want %d (lost updates)", counter, 8*20000)
	}
	if st := *(*uint32)(unsafe.Pointer(m)) & mutexStateMask; st != 0 {
		t.Fatalf("mutex left in state %d", st)
	}
}

// A condition wait must release a recursively held mutex completely, and
// restore its depth afterwards.
func TestCondWaitReleasesRecursiveMutex(t *testing.T) {
	m, unmapM := pin(t)
	defer unmapM()
	c, unmapC := pin(t)
	defer unmapC()
	*(*uint32)(unsafe.Pointer(m)) = 0x4000
	goCondInit(c, 0)
	ready := make(chan struct{})
	done := make(chan int)
	go func() {
		goPthreadMutexLock(m)
		goPthreadMutexLock(m) // depth 2
		close(ready)
		goCondWait(c, m)
		// Still held twice: two unlocks must succeed, a third must not.
		r1 := goPthreadMutexUnlock(m)
		r2 := goPthreadMutexUnlock(m)
		r3 := goPthreadMutexUnlock(m)
		done <- r1 | r2<<8 | r3<<16
	}()
	<-ready
	// The waiter's wait must let us take the mutex.
	deadline := time.Now().Add(5 * time.Second)
	for goPthreadMutexTrylock(m) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("cond wait did not release the recursive mutex")
		}
		time.Sleep(time.Millisecond)
	}
	goCondSignal(c)
	goPthreadMutexUnlock(m)
	// Keep signalling until the waiter is past its wait (a signal sent before
	// it slept is not lost: the generation word changed).
	select {
	case r := <-done:
		if r != errEPERM<<16 {
			t.Fatalf("unlocks after wait: %#x, want two successes then EPERM", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never woke")
	}
}

// A steady_clock wait (CLOCK_MONOTONIC condattr) and a realtime wait must both
// sleep until their deadline and then report ETIMEDOUT.
func TestCondTimedwaitHonoursClock(t *testing.T) {
	for _, clock := range []uintptr{clockMonotonic, clockRealtime} {
		m, unmapM := pin(t)
		c, unmapC := pin(t)
		a, unmapA := pin(t)
		ts, unmapT := pin(t)
		goCondattrInit(a)
		if r := goPthreadCondattrSetclock(a, clock); r != 0 {
			t.Fatalf("setclock(%d): %d", clock, r)
		}
		goCondInit(c, a)
		goPthreadMutexInit(m, 0)

		var now syscall.Timespec
		clk := 1 // CLOCK_MONOTONIC
		if clock == clockRealtime {
			clk = 0
		}
		syscall.Syscall(syscall.SYS_CLOCK_GETTIME, uintptr(clk), uintptr(unsafe.Pointer(&now)), 0)
		deadline := now.Nano() + int64(80*time.Millisecond)
		*(*int64)(unsafe.Pointer(ts)) = deadline / 1e9
		*(*int64)(unsafe.Pointer(ts + 8)) = deadline % 1e9

		goPthreadMutexLock(m)
		start := time.Now()
		r := goCondTimedwait(c, m, ts)
		took := time.Since(start)
		goPthreadMutexUnlock(m)
		unmapM()
		unmapC()
		unmapA()
		unmapT()
		if r != 110 {
			t.Fatalf("clock %d: timedwait = %d, want ETIMEDOUT", clock, r)
		}
		if took < 60*time.Millisecond || took > 2*time.Second {
			t.Fatalf("clock %d: timedwait returned after %v, want ~80ms", clock, took)
		}
	}
}
