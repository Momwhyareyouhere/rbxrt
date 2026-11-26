package hostabi

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"time"
	"unsafe"
)

// AAudio (libaaudio), the NDK audio API FMOD drives on Android, implemented on
// the host's PulseAudio server (PipeWire's pulse service works the same).
//
// FMOD opens an output stream in callback mode: it sets a data callback and
// starts the stream, and the platform then calls the callback from its own
// real-time thread whenever it needs the next burst of samples. Here that
// thread is a goroutine locked to an OS thread, like every guest thread: it
// calls the engine's callback for a burst, then hands the samples to
// pa_simple_write, whose blocking paces the loop at the device rate.
//
// Input (recording) streams are refused with AAUDIO_ERROR_UNAVAILABLE: this
// runtime does not open the microphone.

// aaudio_result_t and enum values (AAudio.h).
const (
	aaudioOK                 = 0
	aaudioErrorDisconnected  = -899
	aaudioErrorIllegalArg    = -898
	aaudioErrorInvalidState  = -895
	aaudioErrorUnavailable   = -889
	aaudioErrorTimeout       = -885
	aaudioErrorInvalidFormat = -883

	aaudioDirectionOutput = 0
	aaudioFormatUnspec    = 0
	aaudioFormatI16       = 1
	aaudioFormatFloat     = 2

	aaudioStateOpen     = 2
	aaudioStateStarting = 3
	aaudioStateStarted  = 4
	aaudioStatePausing  = 5
	aaudioStatePaused   = 6
	aaudioStateStopping = 9
	aaudioStateStopped  = 10
	aaudioStateClosed   = 12
	aaudioStateDisconn  = 13

	aaudioCallbackStop = 1

	// The stream the host opens when the app leaves rate and channels
	// unspecified (FMOD does; it reads them back).
	aaudioDefaultRate     = 48000
	aaudioDefaultChannels = 2
	aaudioBurstFrames     = 480 // 10 ms at 48 kHz
)

type aaudioBuilder struct {
	direction, format, capacity int32
	dataCB, dataUser            uintptr
	errorCB, errorUser          uintptr
}

type aaudioStream struct {
	mu       sync.Mutex
	cond     *sync.Cond
	state    int32
	format   int32
	rate     int32
	channels int32
	burst    int32
	bufSize  int32
	capacity int32

	dataCB, dataUser   uintptr
	errorCB, errorUser uintptr

	pa      uintptr // pa_simple*
	running bool    // the callback thread is alive
	done    chan struct{}
}

var aaudioObjs = struct {
	sync.Mutex
	builders map[uintptr]*aaudioBuilder
	streams  map[uintptr]*aaudioStream
}{builders: map[uintptr]*aaudioBuilder{}, streams: map[uintptr]*aaudioStream{}}

func aaudioStreamOf(h uintptr) *aaudioStream {
	aaudioObjs.Lock()
	defer aaudioObjs.Unlock()
	return aaudioObjs.streams[h]
}

func aaudioBuilderOf(h uintptr) *aaudioBuilder {
	aaudioObjs.Lock()
	defer aaudioObjs.Unlock()
	return aaudioObjs.builders[h]
}

var debugAudio = os.Getenv("RBXRT_DEBUG_AUDIO") != ""

func init() {
	reg := func(name string, fn any) { hostFuncs[name] = fn }
	reg("AAudio_createStreamBuilder", goAAudioCreateStreamBuilder)
	reg("AAudioStreamBuilder_delete", goAAudioBuilderDelete)
	reg("AAudioStreamBuilder_setDirection", goAAudioBuilderSetDirection)
	reg("AAudioStreamBuilder_setFormat", goAAudioBuilderSetFormat)
	reg("AAudioStreamBuilder_setBufferCapacityInFrames", goAAudioBuilderSetCapacity)
	reg("AAudioStreamBuilder_setPerformanceMode", goAAudioBuilderIgnore)
	reg("AAudioStreamBuilder_setUsage", goAAudioBuilderIgnore)
	reg("AAudioStreamBuilder_setInputPreset", goAAudioBuilderIgnore)
	reg("AAudioStreamBuilder_setDataCallback", goAAudioBuilderSetDataCallback)
	reg("AAudioStreamBuilder_setErrorCallback", goAAudioBuilderSetErrorCallback)
	reg("AAudioStreamBuilder_openStream", goAAudioBuilderOpenStream)

	reg("AAudioStream_close", goAAudioStreamClose)
	reg("AAudioStream_requestStart", goAAudioStreamStart)
	reg("AAudioStream_requestPause", goAAudioStreamPause)
	reg("AAudioStream_requestStop", goAAudioStreamStop)
	reg("AAudioStream_getState", goAAudioStreamGetState)
	reg("AAudioStream_waitForStateChange", goAAudioStreamWaitForStateChange)
	reg("AAudioStream_getSampleRate", goAAudioStreamGetSampleRate)
	reg("AAudioStream_getChannelCount", goAAudioStreamGetChannelCount)
	reg("AAudioStream_getFormat", goAAudioStreamGetFormat)
	reg("AAudioStream_getFramesPerBurst", goAAudioStreamGetFramesPerBurst)
	reg("AAudioStream_getBufferSizeInFrames", goAAudioStreamGetBufferSize)
	reg("AAudioStream_getBufferCapacityInFrames", goAAudioStreamGetCapacity)
	reg("AAudioStream_setBufferSizeInFrames", goAAudioStreamSetBufferSize)
	reg("AAudioStream_getXRunCount", goAAudioStreamGetXRunCount)
	reg("AAudioStream_read", goAAudioStreamRead)
}

func audioLog(format string, a ...any) {
	if debugAudio {
		writeStderrRaw(fmt.Sprintf("rbxrt: audio: "+format+"\n", a...))
	}
}

// --- builder --------------------------------------------------------------

//go:noinline
func goAAudioCreateStreamBuilder(out uintptr) int {
	if out == 0 {
		return aaudioErrorIllegalArg
	}
	b := &aaudioBuilder{}
	h := uintptr(unsafe.Pointer(b))
	aaudioObjs.Lock()
	aaudioObjs.builders[h] = b
	aaudioObjs.Unlock()
	*(*uintptr)(unsafe.Pointer(out)) = h
	return aaudioOK
}

//go:noinline
func goAAudioBuilderDelete(h uintptr) int {
	aaudioObjs.Lock()
	delete(aaudioObjs.builders, h)
	aaudioObjs.Unlock()
	return aaudioOK
}

//go:noinline
func goAAudioBuilderSetDirection(h, v uintptr) {
	if b := aaudioBuilderOf(h); b != nil {
		b.direction = int32(v)
	}
}

//go:noinline
func goAAudioBuilderSetFormat(h, v uintptr) {
	if b := aaudioBuilderOf(h); b != nil {
		b.format = int32(v)
	}
}

//go:noinline
func goAAudioBuilderSetCapacity(h, v uintptr) {
	if b := aaudioBuilderOf(h); b != nil {
		b.capacity = int32(v)
	}
}

//go:noinline
func goAAudioBuilderIgnore(h, v uintptr) {}

//go:noinline
func goAAudioBuilderSetDataCallback(h, cb, user uintptr) {
	if b := aaudioBuilderOf(h); b != nil {
		b.dataCB, b.dataUser = cb, user
	}
}

//go:noinline
func goAAudioBuilderSetErrorCallback(h, cb, user uintptr) {
	if b := aaudioBuilderOf(h); b != nil {
		b.errorCB, b.errorUser = cb, user
	}
}

//go:noinline
func goAAudioBuilderOpenStream(h, out uintptr) int {
	b := aaudioBuilderOf(h)
	if b == nil || out == 0 {
		return aaudioErrorIllegalArg
	}
	*(*uintptr)(unsafe.Pointer(out)) = 0
	if b.direction != aaudioDirectionOutput {
		audioLog("input stream refused (no microphone support)")
		return aaudioErrorUnavailable
	}
	format := b.format
	switch format {
	case aaudioFormatUnspec:
		format = aaudioFormatFloat
	case aaudioFormatI16, aaudioFormatFloat:
	default:
		return aaudioErrorInvalidFormat
	}
	s := &aaudioStream{
		state: aaudioStateOpen, format: format,
		rate: aaudioDefaultRate, channels: aaudioDefaultChannels,
		burst: aaudioBurstFrames, bufSize: 2 * aaudioBurstFrames, capacity: 8 * aaudioBurstFrames,
		dataCB: b.dataCB, dataUser: b.dataUser, errorCB: b.errorCB, errorUser: b.errorUser,
	}
	if b.capacity > s.capacity {
		s.capacity = b.capacity
	}
	s.cond = sync.NewCond(&s.mu)
	pa, err := pulseOpen(s)
	if err != nil {
		audioLog("open failed: %v", err)
		return aaudioErrorUnavailable
	}
	s.pa = pa
	sh := uintptr(unsafe.Pointer(s))
	aaudioObjs.Lock()
	aaudioObjs.streams[sh] = s
	aaudioObjs.Unlock()
	*(*uintptr)(unsafe.Pointer(out)) = sh
	audioLog("stream %#x opened: %d Hz, %d ch, format %d, callback %#x", sh, s.rate, s.channels, s.format, s.dataCB)
	return aaudioOK
}

// --- stream ---------------------------------------------------------------

func (s *aaudioStream) frameBytes() int {
	if s.format == aaudioFormatI16 {
		return 2 * int(s.channels)
	}
	return 4 * int(s.channels)
}

func (s *aaudioStream) setState(st int32) {
	s.state = st
	s.cond.Broadcast()
}

//go:noinline
func goAAudioStreamStart(h uintptr) int {
	s := aaudioStreamOf(h)
	if s == nil {
		return aaudioErrorIllegalArg
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.state {
	case aaudioStateStarted, aaudioStateStarting:
		return aaudioOK
	case aaudioStateClosed, aaudioStateDisconn:
		return aaudioErrorInvalidState
	}
	if s.dataCB == 0 {
		// Blocking-write mode is not supported (the engine imports no
		// AAudioStream_write).
		return aaudioErrorInvalidState
	}
	s.setState(aaudioStateStarted)
	if !s.running {
		s.running = true
		s.done = make(chan struct{})
		go s.callbackThread(h)
	}
	audioLog("stream %#x started", h)
	return aaudioOK
}

// stopTo moves a started stream to a paused/stopped state and waits for the
// callback thread to finish its current burst.
func (s *aaudioStream) stopTo(h uintptr, transient, final int32) int {
	s.mu.Lock()
	switch s.state {
	case aaudioStateClosed:
		s.mu.Unlock()
		return aaudioErrorInvalidState
	case aaudioStateStarted, aaudioStateStarting:
		s.setState(transient)
	}
	done, running := s.done, s.running
	s.mu.Unlock()
	if running && !onCallbackThread(s) {
		<-done
	}
	s.mu.Lock()
	if s.state != aaudioStateDisconn {
		s.setState(final)
	}
	s.mu.Unlock()
	if s.pa != 0 {
		var e int32
		callHostC(pulse.flush, &[9]uintptr{s.pa, uintptr(unsafe.Pointer(&e))})
	}
	audioLog("stream %#x -> state %d", h, final)
	return aaudioOK
}

//go:noinline
func goAAudioStreamPause(h uintptr) int {
	if s := aaudioStreamOf(h); s != nil {
		return s.stopTo(h, aaudioStatePausing, aaudioStatePaused)
	}
	return aaudioErrorIllegalArg
}

//go:noinline
func goAAudioStreamStop(h uintptr) int {
	if s := aaudioStreamOf(h); s != nil {
		return s.stopTo(h, aaudioStateStopping, aaudioStateStopped)
	}
	return aaudioErrorIllegalArg
}

//go:noinline
func goAAudioStreamClose(h uintptr) int {
	s := aaudioStreamOf(h)
	if s == nil {
		return aaudioErrorIllegalArg
	}
	s.stopTo(h, aaudioStateStopping, aaudioStateStopped)
	s.mu.Lock()
	s.setState(aaudioStateClosed)
	pa := s.pa
	s.pa = 0
	s.mu.Unlock()
	if pa != 0 {
		callHostC(pulse.free, &[9]uintptr{pa})
	}
	aaudioObjs.Lock()
	delete(aaudioObjs.streams, h)
	aaudioObjs.Unlock()
	audioLog("stream %#x closed", h)
	return aaudioOK
}

// callbackThreads records which goroutine runs each stream's callback, so a
// stop requested from inside the callback does not wait for itself.
var callbackThreads sync.Map // *aaudioStream -> g

func onCallbackThread(s *aaudioStream) bool {
	g, ok := callbackThreads.Load(s)
	return ok && g.(uintptr) == getg()
}

// callbackThread is the stream's real-time thread: it asks the engine for
// each burst and writes it to the sound server.
func (s *aaudioStream) callbackThread(h uintptr) {
	runtime.LockOSThread() // a guest thread keeps its OS thread, as a pthread does
	g := getg()
	callbackThreads.Store(s, g)
	defer func() {
		callbackThreads.Delete(s)
		s.mu.Lock()
		s.running = false
		close(s.done)
		s.mu.Unlock()
		releaseThreadLocal(g)
	}()
	buf := make([]byte, int(s.burst)*s.frameBytes())
	p := uintptr(unsafe.Pointer(&buf[0]))
	var perr int32
	for {
		s.mu.Lock()
		running := s.state == aaudioStateStarted
		s.mu.Unlock()
		if !running {
			return
		}
		r := CallGuestInt8(s.dataCB, h, s.dataUser, p, uintptr(s.burst), 0, 0, 0, 0)
		if int32(r) == aaudioCallbackStop {
			s.mu.Lock()
			s.setState(aaudioStateStopped)
			s.mu.Unlock()
			return
		}
		if int32(callHostC(pulse.write, &[9]uintptr{s.pa, p, uintptr(len(buf)),
			uintptr(unsafe.Pointer(&perr))})) < 0 {
			audioLog("stream %#x: sound server write failed (error %d); disconnected", h, perr)
			s.mu.Lock()
			s.setState(aaudioStateDisconn)
			s.mu.Unlock()
			if s.errorCB != 0 {
				CallGuestInt3(s.errorCB, h, s.errorUser, aaudioErrorCode(aaudioErrorDisconnected))
			}
			return
		}
	}
}

//go:noinline
func goAAudioStreamGetState(h uintptr) int {
	s := aaudioStreamOf(h)
	if s == nil {
		return aaudioStateClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return int(s.state)
}

// waitForStateChange(stream, inputState, *nextState, timeoutNanos)
//
//go:noinline
func goAAudioStreamWaitForStateChange(h, input, next, timeout uintptr) int {
	s := aaudioStreamOf(h)
	if s == nil {
		return aaudioErrorIllegalArg
	}
	deadline := time.Now().Add(time.Duration(int64(timeout)))
	s.mu.Lock()
	for s.state == int32(input) && time.Now().Before(deadline) {
		s.mu.Unlock()
		time.Sleep(time.Millisecond)
		s.mu.Lock()
	}
	st := s.state
	s.mu.Unlock()
	if next != 0 {
		*(*int32)(unsafe.Pointer(next)) = st
	}
	if st == int32(input) {
		return aaudioErrorTimeout
	}
	return aaudioOK
}

func streamInt(h uintptr, f func(*aaudioStream) int32) int {
	if s := aaudioStreamOf(h); s != nil {
		return int(f(s))
	}
	return aaudioErrorIllegalArg
}

//go:noinline
func goAAudioStreamGetSampleRate(h uintptr) int {
	return streamInt(h, func(s *aaudioStream) int32 { return s.rate })
}

//go:noinline
func goAAudioStreamGetChannelCount(h uintptr) int {
	return streamInt(h, func(s *aaudioStream) int32 { return s.channels })
}

//go:noinline
func goAAudioStreamGetFormat(h uintptr) int {
	return streamInt(h, func(s *aaudioStream) int32 { return s.format })
}

//go:noinline
func goAAudioStreamGetFramesPerBurst(h uintptr) int {
	return streamInt(h, func(s *aaudioStream) int32 { return s.burst })
}

//go:noinline
func goAAudioStreamGetBufferSize(h uintptr) int {
	return streamInt(h, func(s *aaudioStream) int32 { return s.bufSize })
}

//go:noinline
func goAAudioStreamGetCapacity(h uintptr) int {
	return streamInt(h, func(s *aaudioStream) int32 { return s.capacity })
}

// setBufferSizeInFrames returns the size actually set, clamped to
// [one burst, capacity].
//
//go:noinline
func goAAudioStreamSetBufferSize(h, frames uintptr) int {
	return streamInt(h, func(s *aaudioStream) int32 {
		n := int32(frames)
		if n < s.burst {
			n = s.burst
		}
		if n > s.capacity {
			n = s.capacity
		}
		s.bufSize = n
		return n
	})
}

//go:noinline
func goAAudioStreamGetXRunCount(h uintptr) int { return 0 }

// Only output streams exist, so there is nothing to read.
//
//go:noinline
func goAAudioStreamRead(h, buf, frames, timeout uintptr) int { return aaudioErrorInvalidState }

// --- PulseAudio -----------------------------------------------------------

// pulse holds libpulse-simple's entry points.
var pulse struct {
	once                      sync.Once
	err                       error
	newFn, write, flush, free uintptr
	strerror                  uintptr
}

func pulseLoad() error {
	pulse.once.Do(func() {
		lib := hostDlopen("libpulse-simple.so.0", rtldNow|rtldGlobal)
		core := hostDlopen("libpulse.so.0", rtldNow|rtldGlobal)
		if lib == 0 || core == 0 {
			pulse.err = fmt.Errorf("libpulse-simple.so.0 not found (install PulseAudio or pipewire-pulse)")
			return
		}
		pulse.newFn = hostDlsym(lib, "pa_simple_new")
		pulse.write = hostDlsym(lib, "pa_simple_write")
		pulse.flush = hostDlsym(lib, "pa_simple_flush")
		pulse.free = hostDlsym(lib, "pa_simple_free")
		pulse.strerror = hostDlsym(core, "pa_strerror")
		if pulse.newFn == 0 || pulse.write == 0 || pulse.flush == 0 || pulse.free == 0 {
			pulse.err = fmt.Errorf("libpulse-simple is missing pa_simple_* functions")
		}
	})
	return pulse.err
}

// pa_sample_spec and pa_buffer_attr (pulse/sample.h, pulse/def.h).
type paSampleSpec struct {
	format   int32
	rate     uint32
	channels uint8
}

type paBufferAttr struct {
	maxlength, tlength, prebuf, minreq, fragsize uint32
}

// pulseOpen connects a playback stream for s to the default sound device.
func pulseOpen(s *aaudioStream) (uintptr, error) {
	if err := pulseLoad(); err != nil {
		return 0, err
	}
	spec := &paSampleSpec{format: 5, rate: uint32(s.rate), channels: uint8(s.channels)} // PA_SAMPLE_FLOAT32LE
	if s.format == aaudioFormatI16 {
		spec.format = 3 // PA_SAMPLE_S16LE
	}
	burstBytes := uint32(int(s.burst) * s.frameBytes())
	attr := &paBufferAttr{maxlength: ^uint32(0), tlength: 4 * burstBytes, prebuf: ^uint32(0),
		minreq: burstBytes, fragsize: ^uint32(0)}
	var perr int32
	pa := callHostC(pulse.newFn, &[9]uintptr{
		0, CString("Roblox (rbxrt)"), 1, 0, CString("Game audio"), // PA_STREAM_PLAYBACK
		uintptr(unsafe.Pointer(spec)), 0, uintptr(unsafe.Pointer(attr)), uintptr(unsafe.Pointer(&perr))})
	runtime.KeepAlive(spec)
	runtime.KeepAlive(attr)
	if pa == 0 {
		msg := fmt.Sprintf("error %d", perr)
		if pulse.strerror != 0 {
			msg = cstr(callHostC(pulse.strerror, &[9]uintptr{uintptr(perr)}))
		}
		return 0, fmt.Errorf("cannot connect to the sound server: %s", msg)
	}
	return pa, nil
}

// aaudioErrorCode passes a negative aaudio_result_t as an int32 argument.
func aaudioErrorCode(e int32) uintptr { return uintptr(uint32(e)) }
