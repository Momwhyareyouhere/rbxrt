package hostabi

import (
	"math/rand"
	"unsafe"
)

// Data objects the guest addresses directly through GLOB_DAT/ABS relocations.
// In C these are globals; the symbol value is the address of the storage.

// __sF is bionic's array of stdio FILE objects (stdin, stdout, stderr). Each is
// given generous zeroed storage; the guest only ever takes its address and
// passes it back to stdio calls.
var sFStorage [3][512]byte

var stdinPtr = uintptr(unsafe.Pointer(&sFStorage[0][0]))
var stdoutPtr = uintptr(unsafe.Pointer(&sFStorage[1][0]))
var stderrPtr = uintptr(unsafe.Pointer(&sFStorage[2][0]))

var (
	optarg uintptr
	optind = uintptr(1)
	opterr = uintptr(1)
	optopt uintptr
)

var (
	timezoneVar int64 = 0
	tznameVar   [2]uintptr
	daylightVar int32 = 0
)

var (
	in6addrAny      = [16]byte{}
	in6addrLoopback = [16]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}
)

// StackGuard is the value stored in %fs:0x28 for stack-protector functions.
var stackGuard = rand.Uint64()

// MediaFormat string constants exported by libmediandk.
var (
	mediaBitRate      = cstringConst("bitrate")
	mediaChannelCount = cstringConst("channel-count")
	mediaColorFormat  = cstringConst("color-format")
	mediaFrameRate    = cstringConst("frame-rate")
	mediaHeight       = cstringConst("height")
	mediaIFrame       = cstringConst("i-frame-interval")
	mediaMime         = cstringConst("mime")
	mediaSampleRate   = cstringConst("sample-rate")
	mediaStride       = cstringConst("stride")
	mediaWidth        = cstringConst("width")
)

// cstringConst returns the address of a NUL-terminated copy of s. The storage
// lives in the Go heap (which does not move) and is referenced from the symbol
// table, so it stays alive.
func cstringConst(s string) uintptr {
	b := append([]byte(s), 0)
	constStrings = append(constStrings, b)
	return uintptr(unsafe.Pointer(&b[0]))
}

// CString returns the address of a NUL-terminated copy of s that remains alive
// for the lifetime of the process. Used for dli_fname/dlpi_name fields that a
// guest may read at any time.
func CString(s string) uintptr { return cstringConst(s) }

var constStrings [][]byte

// DataSymbols returns the address of each data object the host provides.
func DataSymbols() map[string]uintptr {
	rebuildEnviron()
	return map[string]uintptr{
		"__sF":              uintptr(unsafe.Pointer(&sFStorage[0][0])),
		"stdin":             uintptr(unsafe.Pointer(&stdinPtr)),
		"stdout":            uintptr(unsafe.Pointer(&stdoutPtr)),
		"stderr":            uintptr(unsafe.Pointer(&stderrPtr)),
		"environ":           uintptr(unsafe.Pointer(&environSym)),
		"__stack_chk_guard": uintptr(unsafe.Pointer(&stackGuard)),
		"optarg":            uintptr(unsafe.Pointer(&optarg)),
		"optind":            uintptr(unsafe.Pointer(&optind)),
		"opterr":            uintptr(unsafe.Pointer(&opterr)),
		"optopt":            uintptr(unsafe.Pointer(&optopt)),
		"timezone":          uintptr(unsafe.Pointer(&timezoneVar)),
		"tzname":            uintptr(unsafe.Pointer(&tznameVar[0])),
		"daylight":          uintptr(unsafe.Pointer(&daylightVar)),
		"in6addr_any":       uintptr(unsafe.Pointer(&in6addrAny[0])),
		"in6addr_loopback":  uintptr(unsafe.Pointer(&in6addrLoopback[0])),

		"AMEDIAFORMAT_KEY_BIT_RATE":         uintptr(unsafe.Pointer(&mediaBitRate)),
		"AMEDIAFORMAT_KEY_CHANNEL_COUNT":    uintptr(unsafe.Pointer(&mediaChannelCount)),
		"AMEDIAFORMAT_KEY_COLOR_FORMAT":     uintptr(unsafe.Pointer(&mediaColorFormat)),
		"AMEDIAFORMAT_KEY_FRAME_RATE":       uintptr(unsafe.Pointer(&mediaFrameRate)),
		"AMEDIAFORMAT_KEY_HEIGHT":           uintptr(unsafe.Pointer(&mediaHeight)),
		"AMEDIAFORMAT_KEY_I_FRAME_INTERVAL": uintptr(unsafe.Pointer(&mediaIFrame)),
		"AMEDIAFORMAT_KEY_MIME":             uintptr(unsafe.Pointer(&mediaMime)),
		"AMEDIAFORMAT_KEY_SAMPLE_RATE":      uintptr(unsafe.Pointer(&mediaSampleRate)),
		"AMEDIAFORMAT_KEY_STRIDE":           uintptr(unsafe.Pointer(&mediaStride)),
		"AMEDIAFORMAT_KEY_WIDTH":            uintptr(unsafe.Pointer(&mediaWidth)),
	}
}
