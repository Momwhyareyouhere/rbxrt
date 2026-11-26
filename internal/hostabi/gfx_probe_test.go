package hostabi

import (
	"os"
	"runtime"
	"testing"

	// The graphics layer reaches the host through libc's dlopen, which only
	// exists if the process is dynamically linked. Importing net pulls in the
	// cgo runtime resolver, which is what makes the Go toolchain link a test
	// binary against libc. The rbxrt CLI is already dynamic for the same reason.
	_ "net"
)

// TestGraphicsProbe exercises the runtime's own graphics path end to end:
// dlopen the host libs, open an X11 window, get the X11 platform display, choose
// a config, create a context and window surface, make it current, call into
// real Mesa, and swap. It is the layer the engine will drive, without the
// engine. It needs a display, so it is opt-in:
//
//	RBXRT_GFX_TEST=1 go test ./internal/hostabi -run TestGraphicsProbe -v
func TestGraphicsProbe(t *testing.T) {
	if os.Getenv("RBXRT_GFX_TEST") == "" {
		t.Skip("set RBXRT_GFX_TEST=1 to run the graphics probe")
	}
	// GL state (the dispatch table) is thread-local; keep this goroutine on one
	// OS thread for the whole sequence, exactly as the engine's render thread
	// must be.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	dpy, surf := setupProbeContext(t)
	eglSwapBuffers := hostDlsym(gfx.egl, "eglSwapBuffers")

	// glGetString proves the driver is live and dispatch is current on this
	// thread; glClear + eglSwapBuffers exercise the actual pipeline.
	glGetString, _ := resolveGPU("glGetString")
	glClear, _ := resolveGPU("glClear")
	glViewport, _ := resolveGPU("glViewport")
	glGetError, _ := resolveGPU("glGetError")
	for n, a := range map[string]uintptr{"glGetString": glGetString, "glClear": glClear} {
		if a == 0 {
			t.Fatalf("host libGLESv2 is missing %s", n)
		}
	}
	t.Logf("GL vendor=%q renderer=%q version=%q",
		cstr(callHostC(glGetString, &[9]uintptr{0x1F00})),
		cstr(callHostC(glGetString, &[9]uintptr{0x1F01})),
		cstr(callHostC(glGetString, &[9]uintptr{0x1F02})))

	callHostC(glViewport, &[9]uintptr{0, 0, uintptr(gfx.width), uintptr(gfx.height)})
	callHostC(glClear, &[9]uintptr{0x4000 | 0x100}) // COLOR | DEPTH
	if err := callHostC(glGetError, &[9]uintptr{}); err != 0 {
		t.Fatalf("glGetError after clear = 0x%x", err)
	}
	if callHostC(eglSwapBuffers, &[9]uintptr{dpy, surf}) == 0 {
		t.Fatal("eglSwapBuffers failed")
	}
	t.Logf("rendered one frame into X11 window %#x (%dx%d)", gfx.xWindow, gfx.width, gfx.height)
}

// setupProbeContext brings up the graphics layer and makes a GLES 3 context
// current on a window surface, on the calling (locked) OS thread.
func setupProbeContext(t *testing.T) (dpy, surf uintptr) {
	t.Helper()
	dpy, surf, info, err := probeContext()
	if err != nil {
		t.Fatalf("graphics probe: %v", err)
	}
	t.Logf("EGL %s (%s); GL %q %q %q", info.EGLVersion, info.EGLVendor, info.GLVendor, info.GLRenderer, info.GLVersion)
	return dpy, surf
}
