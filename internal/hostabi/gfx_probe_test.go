package hostabi

import (
	"os"
	"runtime"
	"testing"
	"unsafe"

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

	if err := InitGraphics(); err != nil {
		t.Fatalf("InitGraphics: %v", err)
	}

	eglInit := hostDlsym(gfx.egl, "eglInitialize")
	eglChoose := hostDlsym(gfx.egl, "eglChooseConfig")
	eglCreateContext := hostDlsym(gfx.egl, "eglCreateContext")
	eglMakeCurrent := hostDlsym(gfx.egl, "eglMakeCurrent")
	eglSwapBuffers := hostDlsym(gfx.egl, "eglSwapBuffers")
	eglQueryString := hostDlsym(gfx.egl, "eglQueryString")
	for n, a := range map[string]uintptr{
		"eglInitialize": eglInit, "eglChooseConfig": eglChoose,
		"eglCreateContext": eglCreateContext, "eglMakeCurrent": eglMakeCurrent,
		"eglSwapBuffers": eglSwapBuffers,
	} {
		if a == 0 {
			t.Fatalf("host libEGL is missing %s", n)
		}
	}

	dpy := goEglGetDisplay(0)
	if dpy == 0 {
		t.Fatal("eglGetDisplay returned NULL")
	}

	var maj, mnr int32
	if callHostC(eglInit, &[9]uintptr{dpy, uintptr(unsafe.Pointer(&maj)), uintptr(unsafe.Pointer(&mnr))}) == 0 {
		t.Fatal("eglInitialize failed")
	}
	t.Logf("EGL %d.%d, vendor=%q version=%q", maj, mnr,
		cstr(callHostC(eglQueryString, &[9]uintptr{dpy, 0x3053})),
		cstr(callHostC(eglQueryString, &[9]uintptr{dpy, 0x3054})))

	attribs := []int32{0x3033, 0x0004, 0x3040, 0x0004, 0x3024, 8, 0x3023, 8, 0x3022, 8, 0x3038, 0}
	var cfg uintptr
	var n int32
	if callHostC(eglChoose, &[9]uintptr{dpy, uintptr(unsafe.Pointer(&attribs[0])), uintptr(unsafe.Pointer(&cfg)), 1, uintptr(unsafe.Pointer(&n))}) == 0 || n == 0 {
		t.Fatal("eglChooseConfig found no config")
	}

	anw := goANativeWindowFromSurface(0, 0)
	surf := goEglCreateWindowSurface(dpy, cfg, anw, 0)
	if surf == 0 {
		t.Fatal("eglCreateWindowSurface failed")
	}

	ctxAttribs := []int32{0x3098, 2, 0x3038, 0}
	ctx := callHostC(eglCreateContext, &[9]uintptr{dpy, cfg, 0, uintptr(unsafe.Pointer(&ctxAttribs[0]))})
	if ctx == 0 {
		t.Fatal("eglCreateContext failed")
	}
	if callHostC(eglMakeCurrent, &[9]uintptr{dpy, surf, surf, ctx}) == 0 {
		t.Fatal("eglMakeCurrent failed")
	}

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
