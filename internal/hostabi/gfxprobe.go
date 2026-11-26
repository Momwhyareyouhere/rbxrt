package hostabi

import (
	"errors"
	"fmt"
	"unsafe"
)

// GraphicsInfo describes the host graphics stack as the engine will see it.
type GraphicsInfo struct {
	EGLVersion string // e.g. "1.5"
	EGLVendor  string
	GLVendor   string // glGetString(GL_VENDOR)
	GLRenderer string // e.g. "zink Vulkan 1.4(Intel(R) UHD Graphics ...)"
	GLVersion  string // e.g. "OpenGL ES 3.2 Mesa 26.3.1"
}

// probeContext brings up the graphics layer and makes a GLES 3 context current
// on a window surface, on the calling OS thread (which the caller must have
// locked). It is the path the engine takes, without the engine.
func probeContext() (dpy, surf uintptr, info GraphicsInfo, err error) {
	if err = InitGraphics(); err != nil {
		return 0, 0, info, err
	}
	eglInit := hostDlsym(gfx.egl, "eglInitialize")
	eglChoose := hostDlsym(gfx.egl, "eglChooseConfig")
	eglCreateContext := hostDlsym(gfx.egl, "eglCreateContext")
	eglMakeCurrent := hostDlsym(gfx.egl, "eglMakeCurrent")
	eglQueryString := hostDlsym(gfx.egl, "eglQueryString")
	for n, a := range map[string]uintptr{
		"eglInitialize": eglInit, "eglChooseConfig": eglChoose,
		"eglCreateContext": eglCreateContext, "eglMakeCurrent": eglMakeCurrent,
		"eglQueryString": eglQueryString,
	} {
		if a == 0 {
			return 0, 0, info, fmt.Errorf("host libEGL is missing %s", n)
		}
	}
	if dpy = goEglGetDisplay(0); dpy == 0 {
		return 0, 0, info, errors.New("eglGetDisplay returned no display")
	}
	var maj, mnr int32
	if callHostC(eglInit, &[9]uintptr{dpy, uintptr(unsafe.Pointer(&maj)), uintptr(unsafe.Pointer(&mnr))}) == 0 {
		return 0, 0, info, errors.New("eglInitialize failed")
	}
	info.EGLVersion = fmt.Sprintf("%d.%d", maj, mnr)
	info.EGLVendor = cstr(callHostC(eglQueryString, &[9]uintptr{dpy, 0x3053})) // EGL_VENDOR

	// RGBA8 window config with a GLES 3 renderable bit (EGL_OPENGL_ES3_BIT).
	attribs := []int32{0x3033, 0x0004, 0x3040, 0x0040, 0x3024, 8, 0x3023, 8, 0x3022, 8, 0x3038, 0}
	var cfg uintptr
	var n int32
	if callHostC(eglChoose, &[9]uintptr{dpy, uintptr(unsafe.Pointer(&attribs[0])), uintptr(unsafe.Pointer(&cfg)), 1, uintptr(unsafe.Pointer(&n))}) == 0 || n == 0 {
		return 0, 0, info, errors.New("eglChooseConfig found no GLES 3 window config")
	}
	anw := goANativeWindowFromSurface(0, 0)
	if surf = goEglCreateWindowSurface(dpy, cfg, anw, 0); surf == 0 {
		return 0, 0, info, errors.New("eglCreateWindowSurface failed")
	}
	ctxAttribs := []int32{0x3098, 3, 0x3038, 0} // EGL_CONTEXT_CLIENT_VERSION 3
	ctx := callHostC(eglCreateContext, &[9]uintptr{dpy, cfg, 0, uintptr(unsafe.Pointer(&ctxAttribs[0]))})
	if ctx == 0 {
		return 0, 0, info, errors.New("eglCreateContext (GLES 3) failed")
	}
	if callHostC(eglMakeCurrent, &[9]uintptr{dpy, surf, surf, ctx}) == 0 {
		return 0, 0, info, errors.New("eglMakeCurrent failed")
	}
	glGetString, _ := resolveGPU("glGetString")
	if glGetString == 0 {
		return 0, 0, info, errors.New("host libGLESv2 is missing glGetString")
	}
	info.GLVendor = cstr(callHostC(glGetString, &[9]uintptr{0x1F00}))
	info.GLRenderer = cstr(callHostC(glGetString, &[9]uintptr{0x1F01}))
	info.GLVersion = cstr(callHostC(glGetString, &[9]uintptr{0x1F02}))
	return dpy, surf, info, nil
}

// ProbeGraphics opens the runtime's window (Wayland or X11) and creates a GLES 3 context on
// the host's EGL, reporting what the engine will render with. The calling
// goroutine must be locked to its OS thread (GL state is per thread).
func ProbeGraphics() (GraphicsInfo, error) {
	_, _, info, err := probeContext()
	return info, err
}
