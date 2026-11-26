package hostabi

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"
)

// Debug-only (RBXRT_DEBUG_GFX) call counters for selected GL entry points.
// A Go wrapper cannot stand in for a function with more than seven integer
// arguments (glBlitFramebuffer has ten), so each counted function gets a
// machine-code trampoline that bumps its counter and tail-jumps to the real
// implementation, leaving every argument register and the stack untouched:
//
//	movabsq $counter, %r11
//	lock incq (%r11)
//	movabsq $target, %r11
//	jmpq *%r11
//
// (%r11 is a scratch register in the SysV ABI.)
var countTrampolineCode = []byte{
	0x49, 0xbb, 0x88, 0x77, 0x66, 0x55, 0x44, 0x33, 0x22, 0x11, 0xf0, 0x49,
	0xff, 0x03, 0x49, 0xbb, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88,
	0x41, 0xff, 0xe3,
}

const (
	countTrampolineCounter = 2
	countTrampolineTarget  = 16
)

// countedGLNames are the calls worth counting to see what a frame does.
var countedGLNames = map[string]bool{
	"glDrawArrays": true, "glDrawElements": true,
	"glDrawArraysInstanced": true, "glDrawElementsInstanced": true,
	"glBlitFramebuffer": true, "glBindFramebuffer": true, "glClear": true,
	"glInvalidateFramebuffer": true, "glUseProgram": true,
}

var glCount struct {
	mu       sync.Mutex
	counters map[string]*int64
	tramps   map[uintptr]uintptr // target -> trampoline
}

// countedGL returns a counting trampoline for target when name is counted,
// and target itself otherwise.
func countedGL(name string, target uintptr) uintptr {
	if target == 0 || !countedGLNames[name] {
		return target
	}
	glCount.mu.Lock()
	defer glCount.mu.Unlock()
	if glCount.tramps == nil {
		glCount.tramps = map[uintptr]uintptr{}
		glCount.counters = map[string]*int64{}
	}
	if t, ok := glCount.tramps[target]; ok {
		return t
	}
	c := glCount.counters[name]
	if c == nil {
		c = new(int64) // kept reachable by the map
		glCount.counters[name] = c
	}
	b := codeArena(len(countTrampolineCode))
	copy(b, countTrampolineCode)
	put64(b[countTrampolineCounter:], uintptr(unsafe.Pointer(c)))
	put64(b[countTrampolineTarget:], target)
	arenaMakeExec()
	t := uintptr(unsafe.Pointer(&b[0]))
	glCount.tramps[target] = t
	return t
}

// glCallCounts reports and resets the counters, e.g. "glClear=60 glDrawElements=420".
func glCallCounts() string {
	glCount.mu.Lock()
	defer glCount.mu.Unlock()
	names := make([]string, 0, len(glCount.counters))
	for n := range glCount.counters {
		names = append(names, n)
	}
	sort.Strings(names)
	var parts []string
	for _, n := range names {
		if v := atomic.SwapInt64(glCount.counters[n], 0); v != 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", n, v))
		}
	}
	if len(parts) == 0 {
		return "no counted GL calls"
	}
	return strings.Join(parts, " ")
}
