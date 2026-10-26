package hostabi

import (
	"sync"
	"syscall"
	"unsafe"
)

// stdio. bionic's FILE is opaque to the guest: it only ever takes the address
// it was handed (from fopen, or the __sF slots exposed as stdin/stdout/stderr)
// and passes it back. We therefore represent a stream as a *cFile and key the
// registry on that address, with the three standard streams pre-registered at
// the addresses the data symbols expose.
type cFile struct {
	mu     sync.Mutex
	fd     int
	eof    bool
	err    bool
	owned  bool // fclose should close fd
	ungot  int  // pushback byte, or -1
	append bool
}

var (
	fileRegMu sync.Mutex
	fileReg   = map[uintptr]*cFile{}
)

func init() {
	registerStdio()
}

// registerStdio wires the three standard streams. They are also registered by
// stdioInit, which is called from DataSymbols once the addresses exist.
func registerStdio() {
	fileRegMu.Lock()
	defer fileRegMu.Unlock()
	if _, ok := fileReg[stdinPtr]; !ok {
		fileReg[stdinPtr] = &cFile{fd: 0, ungot: -1}
	}
	if _, ok := fileReg[stdoutPtr]; !ok {
		fileReg[stdoutPtr] = &cFile{fd: 1, ungot: -1}
	}
	if _, ok := fileReg[stderrPtr]; !ok {
		fileReg[stderrPtr] = &cFile{fd: 2, ungot: -1}
	}
}

func registerFile(cf *cFile) uintptr {
	p := uintptr(unsafe.Pointer(cf))
	fileRegMu.Lock()
	fileReg[p] = cf
	fileRegMu.Unlock()
	return p
}

func lookupFile(p uintptr) *cFile {
	fileRegMu.Lock()
	cf := fileReg[p]
	fileRegMu.Unlock()
	return cf
}

func dropFile(p uintptr) {
	fileRegMu.Lock()
	delete(fileReg, p)
	fileRegMu.Unlock()
}

// --- open / close --------------------------------------------------------

func openFlags(mode string) (int, bool) {
	plus := false
	for i := 0; i < len(mode); i++ {
		if mode[i] == '+' {
			plus = true
		}
	}
	switch {
	case len(mode) > 0 && mode[0] == 'r':
		if plus {
			return syscall.O_RDWR, true
		}
		return syscall.O_RDONLY, true
	case len(mode) > 0 && mode[0] == 'w':
		if plus {
			return syscall.O_RDWR | syscall.O_CREAT | syscall.O_TRUNC, true
		}
		return syscall.O_WRONLY | syscall.O_CREAT | syscall.O_TRUNC, true
	case len(mode) > 0 && mode[0] == 'a':
		if plus {
			return syscall.O_RDWR | syscall.O_CREAT | syscall.O_APPEND, true
		}
		return syscall.O_WRONLY | syscall.O_CREAT | syscall.O_APPEND, true
	}
	return 0, false
}

//go:noinline
func goFopen(path, mode uintptr) uintptr {
	flags, ok := openFlags(cstr(mode))
	if !ok {
		return 0
	}
	fd := rawSysNoEnter(2, path, uintptr(flags), 0666)
	debugAssetFileOpen("fopen", path, fd)
	if int64(fd) < 0 {
		return 0
	}
	return registerFile(&cFile{fd: int(fd), owned: true, ungot: -1, append: flags&syscall.O_APPEND != 0})
}

//go:noinline
func goFdopen(fd, mode uintptr) uintptr {
	if _, ok := openFlags(cstr(mode)); !ok {
		return 0
	}
	return registerFile(&cFile{fd: int(fd), ungot: -1})
}

//go:noinline
func goFclose(stream uintptr) int {
	cf := lookupFile(stream)
	if cf == nil {
		return -1
	}
	cf.mu.Lock()
	defer cf.mu.Unlock()
	if cf.owned {
		rawSysNoEnter(3, uintptr(cf.fd), 0, 0)
	}
	dropFile(stream)
	return 0
}

//go:noinline
func goFflush(stream uintptr) int {
	if stream == 0 {
		return 0
	}
	if lookupFile(stream) == nil {
		return -1
	}
	return 0
}

// --- read / write --------------------------------------------------------

func fileRead(cf *cFile, buf []byte) int {
	cf.mu.Lock()
	defer cf.mu.Unlock()
	n := 0
	if cf.ungot >= 0 && len(buf) > 0 {
		buf[0] = byte(cf.ungot)
		cf.ungot = -1
		n = 1
	}
	for n < len(buf) {
		r := rawSysNoEnter(0, uintptr(cf.fd), uintptr(unsafe.Pointer(&buf[n])), uintptr(len(buf)-n))
		if int64(r) < 0 {
			if int64(r) == -int64(syscall.EINTR) {
				continue
			}
			cf.err = true
			break
		}
		if r == 0 {
			cf.eof = true
			break
		}
		n += int(r)
	}
	return n
}

func fileWrite(cf *cFile, buf []byte) int {
	cf.mu.Lock()
	defer cf.mu.Unlock()
	n := 0
	for n < len(buf) {
		w := rawSysNoEnter(1, uintptr(cf.fd), uintptr(unsafe.Pointer(&buf[n])), uintptr(len(buf)-n))
		if int64(w) < 0 {
			if int64(w) == -int64(syscall.EINTR) {
				continue
			}
			cf.err = true
			break
		}
		if w == 0 {
			break
		}
		n += int(w)
	}
	return n
}

//go:noinline
func goFread(ptr, size, nmemb, stream uintptr) uintptr {
	cf := lookupFile(stream)
	if cf == nil || size == 0 || nmemb == 0 {
		return 0
	}
	total := int(size * nmemb)
	buf := unsafe.Slice((*byte)(unsafe.Pointer(ptr)), total)
	n := fileRead(cf, buf)
	return uintptr(n / int(size))
}

//go:noinline
func goFwrite(ptr, size, nmemb, stream uintptr) uintptr {
	cf := lookupFile(stream)
	if cf == nil || size == 0 || nmemb == 0 {
		return 0
	}
	total := int(size * nmemb)
	buf := unsafe.Slice((*byte)(unsafe.Pointer(ptr)), total)
	n := fileWrite(cf, buf)
	return uintptr(n / int(size))
}

// __fread_chk(buf, buflen, size, nmemb, stream)
//
//go:noinline
func goFreadChk(a1, a2, a3, a4, a5 uintptr) uintptr {
	_ = a2
	return goFread(a1, a3, a4, a5)
}

// __fwrite_chk(buf, buflen, size, nmemb, stream)
//
//go:noinline
func goFwriteChk(a1, a2, a3, a4, a5 uintptr) uintptr {
	_ = a2
	return goFwrite(a1, a3, a4, a5)
}

// --- positioning ---------------------------------------------------------

//go:noinline
func goFseek(stream, off, whence uintptr) int {
	cf := lookupFile(stream)
	if cf == nil {
		return -1
	}
	cf.mu.Lock()
	cf.eof = false
	cf.ungot = -1
	cf.mu.Unlock()
	if r := rawSysNoEnter(8, uintptr(cf.fd), off, whence); int64(r) < 0 {
		return -1
	}
	return 0
}

//go:noinline
func goFtell(stream uintptr) int64 {
	cf := lookupFile(stream)
	if cf == nil {
		return -1
	}
	r := rawSysNoEnter(8, uintptr(cf.fd), 0, 1 /* SEEK_CUR */)
	if int64(r) < 0 {
		return -1
	}
	off := int64(r)
	cf.mu.Lock()
	if cf.ungot >= 0 {
		off--
	}
	cf.mu.Unlock()
	return off
}

// --- character / string I/O ---------------------------------------------

//go:noinline
func goFgetc(stream uintptr) int {
	cf := lookupFile(stream)
	if cf == nil {
		return -1
	}
	var b [1]byte
	if fileRead(cf, b[:]) != 1 {
		return -1
	}
	return int(b[0])
}

//go:noinline
func goFputc(c, stream uintptr) int {
	cf := lookupFile(stream)
	if cf == nil {
		return -1
	}
	b := [1]byte{byte(c)}
	if fileWrite(cf, b[:]) != 1 {
		return -1
	}
	return int(byte(c))
}

//go:noinline
func goUngetc(c, stream uintptr) int {
	cf := lookupFile(stream)
	if cf == nil {
		return -1
	}
	cf.mu.Lock()
	cf.ungot = int(byte(c))
	cf.eof = false
	cf.mu.Unlock()
	return int(byte(c))
}

//go:noinline
func goFgets(buf, size, stream uintptr) uintptr {
	cf := lookupFile(stream)
	if cf == nil || size == 0 {
		return 0
	}
	n := int(size) - 1
	i := 0
	for i < n {
		var b [1]byte
		if fileRead(cf, b[:]) != 1 {
			break
		}
		*(*byte)(unsafe.Pointer(buf + uintptr(i))) = b[0]
		i++
		if b[0] == '\n' {
			break
		}
	}
	if i == 0 {
		return 0
	}
	*(*byte)(unsafe.Pointer(buf + uintptr(i))) = 0
	return buf
}

//go:noinline
func goFputs(s, stream uintptr) int {
	cf := lookupFile(stream)
	if cf == nil {
		return -1
	}
	str := cstr(s)
	fileWrite(cf, []byte(str))
	return len(str)
}

//go:noinline
func goPuts(s uintptr) int {
	str := cstr(s) + "\n"
	cf := lookupFile(stdoutPtr)
	if cf == nil {
		return -1
	}
	fileWrite(cf, []byte(str))
	return len(str)
}

// wide-character variants

//go:noinline
func goFputwc(wc, stream uintptr) int {
	// Encode the rune as UTF-8.
	r := rune(uint32(wc))
	buf := utf8Encode(r)
	cf := lookupFile(stream)
	if cf == nil {
		return -1
	}
	fileWrite(cf, buf)
	return int(wc)
}

//go:noinline
func goGetwc(stream uintptr) int {
	return goFgetc(stream)
}

//go:noinline
func goUngetwc(wc, stream uintptr) int {
	return goUngetc(wc, stream)
}

// --- misc ----------------------------------------------------------------

//go:noinline
func goFeof(stream uintptr) int {
	cf := lookupFile(stream)
	if cf == nil {
		return 0
	}
	cf.mu.Lock()
	e := cf.eof
	cf.mu.Unlock()
	if e {
		return 1
	}
	return 0
}

//go:noinline
func goFerror(stream uintptr) int {
	cf := lookupFile(stream)
	if cf == nil {
		return 0
	}
	cf.mu.Lock()
	e := cf.err
	cf.mu.Unlock()
	if e {
		return 1
	}
	return 0
}

//go:noinline
func goClearerr(stream uintptr) {
	cf := lookupFile(stream)
	if cf == nil {
		return
	}
	cf.mu.Lock()
	cf.eof = false
	cf.err = false
	cf.mu.Unlock()
}

//go:noinline
func goFileno(stream uintptr) int {
	cf := lookupFile(stream)
	if cf == nil {
		return -1
	}
	return cf.fd
}

//go:noinline
func goSetvbuf(stream, buf, mode, size uintptr) int { return 0 }

func utf8Encode(r rune) []byte {
	var b [4]byte
	n := encodeRune(b[:], r)
	return b[:n]
}

func encodeRune(p []byte, r rune) int {
	switch {
	case r < 0x80:
		p[0] = byte(r)
		return 1
	case r < 0x800:
		p[0] = 0xC0 | byte(r>>6)
		p[1] = 0x80 | byte(r)&0x3F
		return 2
	case r < 0x10000:
		p[0] = 0xE0 | byte(r>>12)
		p[1] = 0x80 | byte(r>>6)&0x3F
		p[2] = 0x80 | byte(r)&0x3F
		return 3
	default:
		p[0] = 0xF0 | byte(r>>18)
		p[1] = 0x80 | byte(r>>12)&0x3F
		p[2] = 0x80 | byte(r>>6)&0x3F
		p[3] = 0x80 | byte(r)&0x3F
		return 4
	}
}
