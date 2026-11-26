package hostabi

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"unsafe"
)

// The NDK media layer (libmediandk): AMediaFormat, AMediaCodec, AImageReader,
// AImage and AHardwareBuffer.
//
// AMediaFormat is a real key/value store: the engine builds formats before it
// asks for a codec and reads values back. AMediaCodec reports what an Android
// device without a codec for the requested type reports: the create calls
// return NULL. There is no hardware decoder behind this runtime, so the
// engine's video and hardware-codec paths take the "codec unavailable" branch
// they already have for such devices instead of aborting. AImageReader (the
// surface a decoder renders into) is likewise unsupported, and the per-object
// calls answer an invalid object with the NDK's error codes.

// media_status_t values (NdkMediaError.h).
const (
	amediaOK                 = 0
	amediaErrorUnsupported   = -10002
	amediaErrorInvalidObject = -10003
	amediacodecTryAgainLater = -1 // AMEDIACODEC_INFO_TRY_AGAIN_LATER
)

func init() {
	reg := func(name string, fn any) { hostFuncs[name] = fn }

	reg("AMediaFormat_new", goAMediaFormatNew)
	reg("AMediaFormat_delete", goAMediaFormatDelete)
	reg("AMediaFormat_setInt32", goAMediaFormatSetInt32)
	reg("AMediaFormat_setFloat", goAMediaFormatSetFloat)
	reg("AMediaFormat_setString", goAMediaFormatSetString)
	reg("AMediaFormat_setBuffer", goAMediaFormatSetBuffer)
	reg("AMediaFormat_getInt32", goAMediaFormatGetInt32)
	reg("AMediaFormat_getBuffer", goAMediaFormatGetBuffer)
	reg("AMediaFormat_toString", goAMediaFormatToString)

	reg("AMediaCodec_createDecoderByType", goAMediaCodecCreate)
	reg("AMediaCodec_createEncoderByType", goAMediaCodecCreate)
	reg("AMediaCodec_createCodecByName", goAMediaCodecCreate)
	reg("AMediaCodec_delete", goAMediaStatusInvalid)
	reg("AMediaCodec_configure", goAMediaStatusInvalid)
	reg("AMediaCodec_start", goAMediaStatusInvalid)
	reg("AMediaCodec_stop", goAMediaStatusInvalid)
	reg("AMediaCodec_flush", goAMediaStatusInvalid)
	reg("AMediaCodec_queueInputBuffer", goAMediaStatusInvalid)
	reg("AMediaCodec_releaseOutputBuffer", goAMediaStatusInvalid)
	reg("AMediaCodec_dequeueInputBuffer", goAMediaCodecDequeue)
	reg("AMediaCodec_dequeueOutputBuffer", goAMediaCodecDequeue)
	reg("AMediaCodec_getInputBuffer", goAMediaNull)
	reg("AMediaCodec_getOutputBuffer", goAMediaNull)
	reg("AMediaCodec_getOutputFormat", goAMediaNull)

	reg("AImageReader_newWithUsage", goAImageReaderNewWithUsage)
	reg("AImageReader_getWindow", goAImageReaderGetWindow)
	reg("AImageReader_acquireNextImage", goAImageReaderAcquire)
	reg("AImageReader_delete", goAMediaVoid)
	reg("AImage_delete", goAMediaVoid)
	reg("AImage_getTimestamp", goAMediaStatusInvalid)
	reg("AImage_getHardwareBuffer", goAMediaStatusInvalid)
	reg("AHardwareBuffer_acquire", goAMediaVoid)
	reg("AHardwareBuffer_release", goAMediaVoid)
}

// --- AMediaFormat ---------------------------------------------------------

type mediaFormat struct {
	ints   map[string]int32
	floats map[string]float32
	strs   map[string][]byte // NUL-terminated
	bufs   map[string][]byte
	text   []byte // the last toString result, owned by the format
}

var mediaFormats = struct {
	sync.Mutex
	m map[uintptr]*mediaFormat
}{m: map[uintptr]*mediaFormat{}}

func mediaFormatOf(h uintptr) *mediaFormat {
	mediaFormats.Lock()
	defer mediaFormats.Unlock()
	return mediaFormats.m[h]
}

//go:noinline
func goAMediaFormatNew() uintptr {
	f := &mediaFormat{ints: map[string]int32{}, floats: map[string]float32{},
		strs: map[string][]byte{}, bufs: map[string][]byte{}}
	h := uintptr(unsafe.Pointer(f)) // a stable, unique, non-null handle
	mediaFormats.Lock()
	mediaFormats.m[h] = f
	mediaFormats.Unlock()
	return h
}

//go:noinline
func goAMediaFormatDelete(h uintptr) int {
	mediaFormats.Lock()
	defer mediaFormats.Unlock()
	if _, ok := mediaFormats.m[h]; !ok {
		return amediaErrorInvalidObject
	}
	delete(mediaFormats.m, h)
	return amediaOK
}

//go:noinline
func goAMediaFormatSetInt32(h, name, v uintptr) {
	if f := mediaFormatOf(h); f != nil && name != 0 {
		f.ints[cstr(name)] = int32(v)
	}
}

// setFloat(format, name, float): the float arrives in X0, independently of
// the integer registers.
//
//go:noinline
func goAMediaFormatSetFloat(h, name uintptr, v float32) {
	if f := mediaFormatOf(h); f != nil && name != 0 {
		f.floats[cstr(name)] = v
	}
}

//go:noinline
func goAMediaFormatSetString(h, name, v uintptr) {
	if f := mediaFormatOf(h); f != nil && name != 0 {
		f.strs[cstr(name)] = append([]byte(cstr(v)), 0)
	}
}

//go:noinline
func goAMediaFormatSetBuffer(h, name, data, size uintptr) {
	f := mediaFormatOf(h)
	if f == nil || name == 0 {
		return
	}
	b := make([]byte, size, size+1)
	if size > 0 && data != 0 {
		copy(b, unsafe.Slice((*byte)(unsafe.Pointer(data)), size))
	}
	f.bufs[cstr(name)] = b
}

//go:noinline
func goAMediaFormatGetInt32(h, name, out uintptr) uintptr {
	f := mediaFormatOf(h)
	if f == nil || name == 0 {
		return 0
	}
	v, ok := f.ints[cstr(name)]
	if !ok {
		return 0
	}
	if out != 0 {
		*(*int32)(unsafe.Pointer(out)) = v
	}
	return 1
}

//go:noinline
func goAMediaFormatGetBuffer(h, name, dataOut, sizeOut uintptr) uintptr {
	f := mediaFormatOf(h)
	if f == nil || name == 0 {
		return 0
	}
	b, ok := f.bufs[cstr(name)]
	if !ok {
		return 0
	}
	if dataOut != 0 {
		*(*uintptr)(unsafe.Pointer(dataOut)) = uintptr(unsafe.Pointer(unsafe.SliceData(b[:cap(b)])))
	}
	if sizeOut != 0 {
		*(*uintptr)(unsafe.Pointer(sizeOut)) = uintptr(len(b))
	}
	return 1
}

// toString formats like Android: "mime: string(video/avc), width: int32(1280)".
//
//go:noinline
func goAMediaFormatToString(h uintptr) uintptr {
	f := mediaFormatOf(h)
	if f == nil {
		return 0
	}
	var parts []string
	for k, v := range f.ints {
		parts = append(parts, fmt.Sprintf("%s: int32(%d)", k, v))
	}
	for k, v := range f.floats {
		parts = append(parts, fmt.Sprintf("%s: float(%s)", k, formatFloat32(v)))
	}
	for k, v := range f.strs {
		parts = append(parts, fmt.Sprintf("%s: string(%s)", k, v[:len(v)-1]))
	}
	for k, v := range f.bufs {
		parts = append(parts, fmt.Sprintf("%s: data[%d]", k, len(v)))
	}
	sort.Strings(parts)
	f.text = append([]byte(strings.Join(parts, ", ")), 0)
	return uintptr(unsafe.Pointer(&f.text[0]))
}

func formatFloat32(v float32) string {
	if v == float32(math.Trunc(float64(v))) {
		return fmt.Sprintf("%.1f", v)
	}
	return fmt.Sprintf("%g", v)
}

// --- AMediaCodec ----------------------------------------------------------

// No codec is available for any type or name.
//
//go:noinline
func goAMediaCodecCreate(name uintptr) uintptr {
	if debugGFX {
		writeStderrRaw(fmt.Sprintf("rbxrt: media: codec %q requested: none available\n", cstr(name)))
	}
	return 0
}

//go:noinline
func goAMediaCodecDequeue(codec, a, b uintptr) int { return amediacodecTryAgainLater }

// --- AImageReader / AImage / AHardwareBuffer ------------------------------

// AImageReader_newWithUsage(width, height, format, usage, maxImages, **reader)
//
//go:noinline
func goAImageReaderNewWithUsage(w, h, format, usage, maxImages, out uintptr) int {
	if out != 0 {
		*(*uintptr)(unsafe.Pointer(out)) = 0
	}
	return amediaErrorUnsupported
}

//go:noinline
func goAImageReaderGetWindow(reader, out uintptr) int {
	if out != 0 {
		*(*uintptr)(unsafe.Pointer(out)) = 0
	}
	return amediaErrorInvalidObject
}

//go:noinline
func goAImageReaderAcquire(reader, out uintptr) int {
	if out != 0 {
		*(*uintptr)(unsafe.Pointer(out)) = 0
	}
	return amediaErrorInvalidObject
}

// --- shared answers -------------------------------------------------------

//go:noinline
func goAMediaStatusInvalid(a, b, c uintptr) int { return amediaErrorInvalidObject }

//go:noinline
func goAMediaNull(a, b, c uintptr) uintptr { return 0 }

//go:noinline
func goAMediaVoid(a uintptr) {}
