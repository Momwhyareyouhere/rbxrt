package hostabi

import (
	"sync"
	"unsafe"
)

// Java byte arrays. The engine moves text across JNI as byte[]: it turns a
// Java String into UTF-8 with String.getBytes("UTF-8") and reads the result
// with GetArrayLength/GetByteArrayElements, and it hands Java text the other
// way with NewByteArray/SetByteArrayRegion (e.g. a text box's contents for
// showKeyboard). Without arrays every such string arrives empty.
//
// An array is a handle like the other Java objects here; its bytes live in a
// Go slice that never moves, so GetByteArrayElements can return a pointer
// straight into it (isCopy = false, and Release is a no-op).

// JNI table indices for the byte[] subset.
const (
	jniArrGetLength           = 171
	jniArrNewByte             = 176
	jniArrGetByteElements     = 184
	jniArrReleaseByteElements = 192
	jniArrGetByteRegion       = 200
	jniArrSetByteRegion       = 208
)

var jniArrays = struct {
	sync.Mutex
	m    map[uintptr][]byte
	next uintptr
}{m: map[uintptr][]byte{}, next: 0x3000000}

// newByteArray creates a byte[] holding a copy of b and returns its handle.
func newByteArray(b []byte) uintptr {
	c := make([]byte, len(b), len(b)+1) // +1: never a zero-size allocation
	copy(c, b)
	jniArrays.Lock()
	defer jniArrays.Unlock()
	h := jniArrays.next
	jniArrays.next += 16
	jniArrays.m[h] = c
	return h
}

func byteArray(h uintptr) ([]byte, bool) {
	jniArrays.Lock()
	defer jniArrays.Unlock()
	b, ok := jniArrays.m[h]
	return b, ok
}

// ByteArrayBytes returns a copy of a byte[] the engine passed to Java (nil if
// h is not a known array).
func ByteArrayBytes(h uintptr) []byte {
	b, _ := byteArray(h)
	return append([]byte(nil), b...)
}

// jniArrayFn implements the array range of the JNI table (171..214) for byte
// arrays. Other element types keep the old behaviour: a null/zero result.
func jniArrayFn(id, a1, a2, a3, a4 uintptr) uintptr {
	switch id {
	case jniArrGetLength:
		if b, ok := byteArray(a1); ok {
			return uintptr(len(b))
		}
	case jniArrNewByte:
		if int32(a1) >= 0 {
			return newByteArray(make([]byte, int32(a1)))
		}
	case jniArrGetByteElements:
		if b, ok := byteArray(a1); ok {
			if a2 != 0 {
				st8(a2, 0) // isCopy = JNI_FALSE
			}
			if len(b) == 0 {
				return uintptr(unsafe.Pointer(unsafe.SliceData(b[:1])))
			}
			return uintptr(unsafe.Pointer(&b[0]))
		}
	case jniArrReleaseByteElements:
		return 0
	case jniArrGetByteRegion, jniArrSetByteRegion: // (array, start, len, buf)
		b, ok := byteArray(a1)
		start, n := int(int32(a2)), int(int32(a3))
		if !ok || start < 0 || n < 0 || start+n > len(b) || n == 0 {
			return 0
		}
		buf := unsafe.Slice((*byte)(unsafe.Pointer(a4)), n)
		if id == jniArrGetByteRegion {
			copy(buf, b[start:start+n])
		} else {
			copy(b[start:start+n], buf)
		}
	}
	return 0
}

// jniStringGetBytes answers String.getBytes(...) on a string handle: its
// UTF-8 bytes, which is what the engine asks for ("UTF-8"), as a byte[].
func jniStringGetBytes(h uintptr) (uintptr, bool) {
	jniMu.Lock()
	s, ok := jniStrings[h]
	jniMu.Unlock()
	if !ok {
		return 0, false
	}
	if n := len(s); n > 0 && s[n-1] == 0 {
		s = s[:n-1]
	}
	return newByteArray(s), true
}
