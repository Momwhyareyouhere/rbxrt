package hostabi

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"
)

// This file implements the Android NDK asset API (android/asset_manager.h) on
// top of the Roblox APK itself. On Android the framework hands the native code
// an AAssetManager bound to the APK's assets/, and the engine reads its content
// (shaders, fonts, models) through it. We stand in for the framework: the
// manager reads the same assets out of the APK listed in RBXRT_APK, or, if
// RBXRT_ASSETS names a directory, off the filesystem.
//
// Asset names are resolved in the NDK's namespace, so "content/foo" refers to
// "assets/content/foo" in the APK. Both spellings are tried.
//
// Memory note: an opened asset's bytes are held in a Go slice pinned by the
// handle table, and AAsset_getBuffer returns a pointer into it. The runtime
// inhibits the GC globally (see gcinhibit.go), so that pointer stays valid for
// the asset's lifetime, exactly as an mmapped asset would.

// AAsset open modes (android/asset_manager.h).
const (
	aAssetModeUnknown   = 0
	aAssetModeRandom    = 1
	aAssetModeStreaming = 2
	aAssetModeBuffer    = 3
)

// sysMemfdCreate is memfd_create(2) on x86-64.
const sysMemfdCreate = 319

// mfdCloexec is MFD_CLOEXEC.
const mfdCloexec = 0x0001

var debugAsset = os.Getenv("RBXRT_DEBUG_ASSET") != ""

// aAsset is one open asset. Its bytes are loaded whole; AAsset_read and
// AAsset_seek walk pos through them.
type aAsset struct {
	name string
	data []byte
	pos  int
}

// aAssetManager is the process-wide asset source. It is a singleton because the
// guest only ever holds the one the framework gave it.
type aAssetManager struct {
	mu      sync.Mutex
	apk     *zip.ReadCloser
	entries map[string]*zip.File
	dir     string
	missing map[string]bool
}

var (
	assetMu      sync.Mutex
	assetMgr     *aAssetManager
	assetMgrAddr uintptr
	assetOpen            = map[uintptr]*aAsset{}
	assetNext    uintptr = 0x4000000
)

// assetManager returns the process asset source, opening RBXRT_APK once.
func assetManager() *aAssetManager {
	assetMu.Lock()
	defer assetMu.Unlock()
	if assetMgr != nil {
		return assetMgr
	}
	m := &aAssetManager{entries: map[string]*zip.File{}, missing: map[string]bool{}}
	if apk := os.Getenv("RBXRT_APK"); apk != "" {
		zr, err := zip.OpenReader(apk)
		if err != nil {
			fmt.Fprintf(os.Stderr, "rbxrt: AAssetManager: open %s: %v\n", apk, err)
		} else {
			m.apk = zr
			for _, f := range zr.File {
				m.entries[f.Name] = f
			}
		}
	}
	m.dir = os.Getenv("RBXRT_ASSETS")
	if m.apk == nil && m.dir == "" {
		fmt.Fprintf(os.Stderr, "rbxrt: AAssetManager: no asset source (set RBXRT_APK or RBXRT_ASSETS); asset opens will fail\n")
	}
	assetMgr = m
	assetMgrAddr = uintptr(unsafe.Pointer(m))
	return m
}

// candidates lists the archive names a guest asset path may denote.
func assetCandidates(name string) []string {
	name = strings.TrimPrefix(name, "/")
	c := []string{name}
	// The engine's rbxasset:// URIs are opened without the content/ prefix,
	// while the APK stores those resources below assets/content/.
	if !strings.HasPrefix(name, "content/") {
		c = append(c, "content/"+name)
	}
	if !strings.HasPrefix(name, "assets/") {
		c = append(c, "assets/"+name)
		if !strings.HasPrefix(name, "content/") {
			c = append(c, "assets/content/"+name)
		}
	}
	if p := path.Join("assets", name); p != c[len(c)-1] {
		c = append(c, p)
	}
	return c
}

func (m *aAssetManager) open(name string) *aAsset {
	for _, c := range assetCandidates(name) {
		if m.dir != "" {
			if b, err := os.ReadFile(filepath.Join(m.dir, c)); err == nil {
				return &aAsset{name: c, data: b}
			}
		}
		if f := m.entries[c]; f != nil {
			rc, err := f.Open()
			if err != nil {
				continue
			}
			b, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				continue
			}
			return &aAsset{name: c, data: b}
		}
	}
	m.mu.Lock()
	seen := m.missing[name]
	if !seen {
		m.missing[name] = true
	}
	m.mu.Unlock()
	if !seen {
		fmt.Fprintf(os.Stderr, "rbxrt: AAssetManager_open: asset %q not found%s\n", name, m.sourceSuffix())
	} else if debugAsset {
		fmt.Fprintf(os.Stderr, "rbxrt: AAssetManager_open: asset %q not found (repeat)\n", name)
	}
	return nil
}

func (m *aAssetManager) sourceSuffix() string {
	if m.apk != nil {
		return ""
	}
	return " (no asset source configured)"
}

func assetAt(h uintptr) *aAsset {
	assetMu.Lock()
	defer assetMu.Unlock()
	return assetOpen[h]
}

// --- NDK entry points ----------------------------------------------------

//go:noinline
func goAAssetManagerFromJava(env, obj uintptr) uintptr {
	m := assetManager()
	if debugAsset {
		fmt.Fprintf(os.Stderr, "rbxrt: AAssetManager_fromJava(env=%#x obj=%#x) -> %#x\n", env, obj, assetMgrAddr)
	}
	return uintptr(unsafe.Pointer(m))
}

//go:noinline
func goAAssetManagerOpen(mgr, name, mode uintptr) uintptr {
	m := assetManager()
	a := m.open(cstr(name))
	if a == nil {
		return 0
	}
	assetMu.Lock()
	h := assetNext
	assetNext += 16
	assetOpen[h] = a
	assetMu.Unlock()
	if debugAsset {
		fmt.Fprintf(os.Stderr, "rbxrt: AAssetManager_open(%q mode=%d) -> %#x (%d bytes, resolved %q)\n",
			cstr(name), int(mode), h, len(a.data), a.name)
	}
	return h
}

//go:noinline
func goAAssetGetLength(h uintptr) uintptr {
	a := assetAt(h)
	if a == nil {
		return 0
	}
	return uintptr(int64(len(a.data)))
}

//go:noinline
func goAAssetGetBuffer(h uintptr) uintptr {
	a := assetAt(h)
	if a == nil || len(a.data) == 0 {
		return 0
	}
	return uintptr(unsafe.Pointer(&a.data[0]))
}

//go:noinline
func goAAssetRead(h, buf, count uintptr) int {
	a := assetAt(h)
	if a == nil {
		return -1
	}
	n := copy(unsafe.Slice((*byte)(unsafe.Pointer(buf)), int(count)), a.data[a.pos:])
	a.pos += n
	return n
}

//go:noinline
func goAAssetSeek(h, offset, whence uintptr) int64 {
	a := assetAt(h)
	if a == nil {
		return -1
	}
	off := int64(offset)
	switch int(whence) {
	case 0: // SEEK_SET
		a.pos = int(off)
	case 1: // SEEK_CUR
		a.pos += int(off)
	case 2: // SEEK_END
		a.pos = len(a.data) + int(off)
	default:
		return -1
	}
	if a.pos < 0 {
		a.pos = 0
	}
	if a.pos > len(a.data) {
		a.pos = len(a.data)
	}
	return int64(a.pos)
}

//go:noinline
func goAAssetGetRemainingLength(h uintptr) uintptr {
	a := assetAt(h)
	if a == nil {
		return 0
	}
	rem := len(a.data) - a.pos
	if rem < 0 {
		rem = 0
	}
	return uintptr(int64(rem))
}

//go:noinline
func goAAssetClose(h uintptr) {
	assetMu.Lock()
	delete(assetOpen, h)
	assetMu.Unlock()
}

// goAAssetOpenFileDescriptor materialises the asset into an anonymous file and
// returns its descriptor with the mapped window [0, len). The engine uses this
// to mmap an asset without copying, which memfd backs identically to a real
// APK entry.
//
//go:noinline
func goAAssetOpenFileDescriptor(h, outStart, outLength uintptr) int {
	a := assetAt(h)
	if a == nil {
		return -1
	}
	fd := memfdFromBytes(a.name, a.data)
	if fd < 0 {
		return -1
	}
	if outStart != 0 {
		st64(outStart, 0)
	}
	if outLength != 0 {
		st64(outLength, uint64(len(a.data)))
	}
	if debugAsset {
		fmt.Fprintf(os.Stderr, "rbxrt: AAsset_openFileDescriptor(%q) -> fd=%d len=%d\n", a.name, fd, len(a.data))
	}
	return fd
}

// memfdFromBytes creates an anonymous file holding b and returns its descriptor.
func memfdFromBytes(name string, b []byte) int {
	nb := append([]byte(name), 0)
	fd := rawSysNoEnter(sysMemfdCreate, uintptr(unsafe.Pointer(&nb[0])), mfdCloexec, 0)
	if int64(fd) < 0 {
		setErrno(int32(-int64(fd)))
		fmt.Fprintf(os.Stderr, "rbxrt: memfd_create: errno=%d\n", -int64(fd))
		return -1
	}
	for len(b) > 0 {
		n := rawSysNoEnter(1, fd, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)))
		if int64(n) < 0 {
			setErrno(int32(-int64(n)))
			rawSysNoEnter(3, fd, 0, 0)
			return -1
		}
		b = b[int(n):]
	}
	rawSysNoEnter(8, fd, 0, 0)
	return int(fd)
}

func init() {
	hostFuncs["AAssetManager_fromJava"] = goAAssetManagerFromJava
	hostFuncs["AAssetManager_open"] = goAAssetManagerOpen
	hostFuncs["AAsset_getLength"] = goAAssetGetLength
	hostFuncs["AAsset_getLength64"] = goAAssetGetLength
	hostFuncs["AAsset_getRemainingLength"] = goAAssetGetRemainingLength
	hostFuncs["AAsset_getBuffer"] = goAAssetGetBuffer
	hostFuncs["AAsset_isAllocated"] = func(h uintptr) int { return 1 }
	hostFuncs["AAsset_read"] = goAAssetRead
	hostFuncs["AAsset_seek"] = goAAssetSeek
	hostFuncs["AAsset_close"] = goAAssetClose
	hostFuncs["AAsset_openFileDescriptor"] = goAAssetOpenFileDescriptor
	hostFuncs["AAsset_openFileDescriptor64"] = goAAssetOpenFileDescriptor
}
