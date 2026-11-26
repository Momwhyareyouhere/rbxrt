package hostabi

import (
	"math"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// fakeAudioCallback stands in for FMOD's AAudio data callback: it fills the
// burst with a quiet tone's worth of zeros and counts the frames it was asked
// for. It is reached through a host thunk, as guest code would be.
var fakeAudioFrames atomic.Int64

//go:noinline
func fakeAudioCallback(stream, user, data, frames uintptr) int {
	for i := uintptr(0); i < frames*2; i++ { // stereo float32
		*(*float32)(unsafe.Pointer(data + i*4)) = 0
	}
	fakeAudioFrames.Add(int64(frames))
	return 0 // AAUDIO_CALLBACK_RESULT_CONTINUE
}

// TestAAudioPlayback opens an output stream the way FMOD does (callback mode,
// unspecified rate/channels), plays a second of silence through the sound
// server and checks the callback is paced at the device rate.
func TestAAudioPlayback(t *testing.T) {
	if !hostDlAvailable() || pulseLoad() != nil {
		t.Skip("no PulseAudio client library")
	}
	cb := mkThunk(funcAddr(fakeAudioCallback))
	arenaMakeExec()

	var b uintptr
	if r := goAAudioCreateStreamBuilder(uintptr(unsafe.Pointer(&b))); r != aaudioOK {
		t.Fatalf("createStreamBuilder = %d", r)
	}
	goAAudioBuilderSetDirection(b, aaudioDirectionOutput)
	goAAudioBuilderSetFormat(b, aaudioFormatFloat)
	goAAudioBuilderSetDataCallback(b, cb, 0)
	var s uintptr
	if r := goAAudioBuilderOpenStream(b, uintptr(unsafe.Pointer(&s))); r != aaudioOK {
		t.Skipf("openStream = %d (no sound server?)", r)
	}
	goAAudioBuilderDelete(b)
	if rate, ch := goAAudioStreamGetSampleRate(s), goAAudioStreamGetChannelCount(s); rate != 48000 || ch != 2 {
		t.Fatalf("stream is %d Hz %d ch, want 48000 Hz stereo", rate, ch)
	}

	fakeAudioFrames.Store(0)
	if r := goAAudioStreamStart(s); r != aaudioOK {
		t.Fatalf("requestStart = %d", r)
	}
	var next int32
	goAAudioStreamWaitForStateChange(s, aaudioStateStarting, uintptr(unsafe.Pointer(&next)), uintptr(time.Second))
	if next != aaudioStateStarted {
		t.Fatalf("state after start = %d, want started", next)
	}
	time.Sleep(aaudioTestDuration)
	if r := goAAudioStreamStop(s); r != aaudioOK {
		t.Fatalf("requestStop = %d", r)
	}
	frames := fakeAudioFrames.Load()
	// One second is 48000 frames; the server takes ~150 ms to start
	// consuming and first fills its buffer.
	if frames < 36000 || frames > 60000 {
		t.Errorf("callback produced %d frames in 1 s, want about 48000 (paced by the device)", frames)
	}
	if st := goAAudioStreamGetState(s); st != aaudioStateStopped {
		t.Errorf("state after stop = %d, want stopped", st)
	}
	if r := goAAudioStreamClose(s); r != aaudioOK {
		t.Errorf("close = %d", r)
	}
	t.Logf("callback produced %d frames in 1 s", frames)
}

func TestAAudioRefusesInput(t *testing.T) {
	var b, s uintptr
	goAAudioCreateStreamBuilder(uintptr(unsafe.Pointer(&b)))
	goAAudioBuilderSetDirection(b, 1) // AAUDIO_DIRECTION_INPUT
	if r := goAAudioBuilderOpenStream(b, uintptr(unsafe.Pointer(&s))); r != aaudioErrorUnavailable || s != 0 {
		t.Fatalf("input stream: result %d stream %#x, want unavailable and NULL", r, s)
	}
}

func TestMediaFormat(t *testing.T) {
	f := goAMediaFormatNew()
	goAMediaFormatSetString(f, CString("mime"), CString("video/avc"))
	goAMediaFormatSetInt32(f, CString("width"), 1280)
	goAMediaFormatSetFloat(f, CString("frame-rate"), 29.97)
	data := []byte{0, 0, 0, 1, 0x67}
	goAMediaFormatSetBuffer(f, CString("csd-0"), uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)))

	var w int32
	if goAMediaFormatGetInt32(f, CString("width"), uintptr(unsafe.Pointer(&w))) != 1 || w != 1280 {
		t.Errorf("width = %d", w)
	}
	if goAMediaFormatGetInt32(f, CString("height"), uintptr(unsafe.Pointer(&w))) != 0 {
		t.Errorf("missing key reported present")
	}
	var p, n uintptr
	if goAMediaFormatGetBuffer(f, CString("csd-0"), uintptr(unsafe.Pointer(&p)), uintptr(unsafe.Pointer(&n))) != 1 ||
		n != 5 || *(*byte)(unsafe.Pointer(p + 4)) != 0x67 {
		t.Errorf("csd-0 buffer wrong (n=%d)", n)
	}
	want := "csd-0: data[5], frame-rate: float(29.97), mime: string(video/avc), width: int32(1280)"
	if got := cstr(goAMediaFormatToString(f)); got != want {
		t.Errorf("toString = %q, want %q", got, want)
	}
	if goAMediaFormatDelete(f) != amediaOK || goAMediaFormatDelete(f) != amediaErrorInvalidObject {
		t.Errorf("delete twice: want OK then invalid object")
	}
	if goAMediaCodecCreate(CString("video/avc")) != 0 {
		t.Errorf("a codec was created; none exists")
	}
	var r uintptr = 1
	if goAImageReaderNewWithUsage(64, 64, 1, 0, 2, uintptr(unsafe.Pointer(&r))) != amediaErrorUnsupported || r != 0 {
		t.Errorf("AImageReader: want unsupported and NULL")
	}
}

func TestLibcAdditions(t *testing.T) {
	// ldiv's native stub: ldiv(-7, 2) = {quot -3, rem -1}.
	quot := CallHostRaw(rawSyms["ldiv"], uintptr(0xfffffffffffffff9), 2, 0)
	if int64(quot) != -3 {
		t.Errorf("ldiv(-7,2).quot = %d, want -3", int64(quot))
	}
	if idx := goIfNametoindex(CString("lo")); idx == 0 {
		t.Errorf("if_nametoindex(lo) = 0")
	} else {
		buf := make([]byte, 16)
		if goIfIndextoname(idx, uintptr(unsafe.Pointer(&buf[0]))) == 0 || cstr(uintptr(unsafe.Pointer(&buf[0]))) != "lo" {
			t.Errorf("if_indextoname(%d) != lo", idx)
		}
	}
	if goScalbn(1.5, 3) != 12 {
		t.Errorf("scalbn(1.5, 3) = %v", goScalbn(1.5, 3))
	}

	// Two control messages of 20 and 16 bytes in a 40-byte buffer.
	ctl := make([]byte, 40)
	*(*uint64)(unsafe.Pointer(&ctl[0])) = 20
	*(*uint64)(unsafe.Pointer(&ctl[24])) = 16
	msg := make([]byte, 56)
	*(*uintptr)(unsafe.Pointer(&msg[32])) = uintptr(unsafe.Pointer(&ctl[0]))
	*(*uint64)(unsafe.Pointer(&msg[40])) = 40
	m, c := uintptr(unsafe.Pointer(&msg[0])), uintptr(unsafe.Pointer(&ctl[0]))
	if next := goCmsgNxthdr(m, c); next != c+24 {
		t.Errorf("first CMSG_NXTHDR = +%d, want +24", next-c)
	} else if goCmsgNxthdr(m, next) != 0 {
		t.Errorf("CMSG_NXTHDR past the last message is not NULL")
	}
}

func TestThermal(t *testing.T) {
	if goAThermalAcquireManager() == 0 {
		t.Fatal("no thermal manager")
	}
	h := goAThermalGetThermalHeadroom(0, 0)
	st := goAThermalGetCurrentThermalStatus(0)
	if !math.IsNaN(float64(h)) && (h <= 0 || h > 2) {
		t.Errorf("headroom %v out of range", h)
	}
	t.Logf("headroom %.2f, status %d", h, st)
}

func TestVirtualLibraries(t *testing.T) {
	h := VirtualLibrary("/system/lib64/libaaudio.so")
	if h == 0 || !IsVirtualLibrary(h) {
		t.Fatalf("libaaudio.so is not provided")
	}
	if VirtualLibrary("libvulkan.so") != 0 {
		t.Errorf("libvulkan.so claimed")
	}
	if _, ok := VirtualSymbol("AAudio_createStreamBuilder"); !ok {
		t.Errorf("AAudio_createStreamBuilder not found")
	}
	if _, ok := VirtualSymbol("AAudioStream_write_doesnotexist"); ok {
		t.Errorf("an unimplemented name resolved")
	}
}

var aaudioTestDuration = time.Second
