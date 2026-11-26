package hostabi

import (
	"fmt"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"unsafe"
)

// This file provides a host-side Java Native Interface environment. The guest
// (libroblox.so) is an Android JNI library: its real entry point is
// JNI_OnLoad(JavaVM*, void*), and the Java application drives the engine
// through the 557 Java_* native methods the library exports. A bare Linux host
// has no ART or Dalvik, so the runtime supplies the VM and environment the
// guest expects.
//
// The environment is two C-style interface tables:
//
//	JNINativeInterface (JNIEnv)  — 256 entries, offsets 0x00..0x7f8
//	JNIInvokeInterface (JavaVM)  — 8 entries,   offsets 0x00..0x38
//
// Every table entry is a trampoline that loads a function id into R10 and jumps
// to jniEntry (see abi_amd64.s), which saves the guest's SysV argument registers
// and calls goJNIEntry. A single dispatcher therefore covers every JNI function
// whatever its signature.
//
// Java classes, objects, method ids and field ids are opaque to native code, so
// the runtime represents them as small non-null integer handles and remembers
// what they denote. Nothing here invents Java behaviour: an unimplemented JNI
// call is logged once and returns a null/zero result, and what the guest does
// with that result is what actually drives progress.

// jniVersion16 is JNI_VERSION_1_6, the version Android reports and the version
// JNI_OnLoad asks for (GetEnv was called with 0x10006).
const jniVersion16 = 0x00010006

// JNINativeInterface indices (bionic ordering). Only the entries the host
// implements are named; the dispatcher handles whole ranges generically.
const (
	jniGetVersion              = 4
	jniFindClass               = 6
	jniExceptionOccurred       = 15
	jniExceptionClear          = 17
	jniNewGlobalRef            = 21
	jniDeleteGlobalRef         = 22
	jniDeleteLocalRef          = 23
	jniIsSameObject            = 24
	jniNewLocalRef             = 25
	jniAllocObject             = 27
	jniNewObject               = 28
	jniNewObjectV              = 29
	jniNewObjectA              = 30
	jniGetObjectClass          = 31
	jniIsInstanceOf            = 32
	jniGetMethodID             = 33
	jniGetFieldID              = 94
	jniGetStaticMethodID       = 113
	jniGetStaticFieldID        = 144
	jniNewString               = 163
	jniNewStringUTF            = 167
	jniGetStringUTFLength      = 168
	jniGetStringUTFChars       = 169
	jniReleaseStringUTFChars   = 170
	jniGetArrayLength          = 171
	jniRegisterNatives         = 215
	jniUnregisterNatives       = 216
	jniMonitorEnter            = 217
	jniMonitorExit             = 218
	jniGetJavaVM               = 219
	jniNewWeakGlobalRef        = 226
	jniDeleteWeakGlobalRef     = 227
	jniExceptionCheck          = 228
	jniNewDirectByteBuffer     = 229
	jniGetDirectBufferAddress  = 230
	jniGetDirectBufferCapacity = 231
	jniGetObjectRefType        = 232
)

// JNIInvokeInterface (JavaVM) indices.
const (
	jniDestroyJavaVM               = 3
	jniAttachCurrentThread         = 4
	jniDetachCurrentThread         = 5
	jniGetEnv                      = 6
	jniAttachCurrentThreadAsDaemon = 7
)

const (
	jniEnvTableLen = 256
	jniVMTableLen  = 8
	// VM trampolines use a disjoint id range so the dispatcher can tell which
	// interface a call arrived through.
	jniVMIDBase = 0x1000
)

// jniTarget is goJNIEntry's ABIInternal entry, published for assembly. A direct
// symbol reference from asm would bind to the ABI0 wrapper (see variadicTarget).
var jniTarget uintptr

func init() { jniTarget = funcAddr(goJNIEntry) }

// The JNI tables are built eagerly at package init: constructing them lazily
// (on the first JavaVM()/JNIEnvAddr() call) would rewrite the code arena while
// guest threads created by the constructors are already executing thunks, and
// nothing may render a live, guest-callable arena non-executable even for an
// instant (see codeArena). Building here keeps the tables fixed before main
// runs and is cheap (264 x 32-byte trampolines).
func init() { jniOnce.Do(jniInit) }

// jniEntryAddr and jniEntry are implemented in abi_amd64.s.
func jniEntryAddr() uintptr

var (
	jniOnce sync.Once

	// jniEnvTable/jniVMTable are the interface tables; the *Slot globals hold
	// each table's base address, and jniEnvAddr/jniVMAddr are the guest-visible
	// JNIEnv* / JavaVM* (JNI hands these out as pointers to a pointer to the
	// table).
	jniEnvTable []uintptr
	jniVMTable  []uintptr
	jniEnvSlot  uintptr
	jniVMSlot   uintptr
	jniEnvAddr  uintptr
	jniVMAddr   uintptr
)

// JavaVM returns a guest-visible JavaVM* for JNI_OnLoad, building the interface
// tables on first use.
func JavaVM() uintptr {
	jniOnce.Do(jniInit)
	return jniVMAddr
}

// JNIEnvAddr returns the guest-visible JNIEnv* the VM hands out for the
// current thread.
//
// A guest thread is modelled as a goroutine, and a JNIEnv is thread-specific:
// each thread must observe its own *env slot. This matters because the engine's
// generated bindings cache the interface table in *env — function 0x6d25f14
// writes *env = <per-thread table> after copying the caller's table into its
// own storage. With one shared slot, the first thread to run that code replaced
// every other thread's view of the table, so a later thread dereferenced the
// wrong entry (slot 228, which the generated table points back at the telemetry
// wrapper) and recursed into it until the re-entrancy guard fired _Exit(1000).
// A distinct slot per thread isolates that write.
func JNIEnvAddr() uintptr {
	jniOnce.Do(jniInit)
	return envSlotForCurrentThread()
}

// envSlotByG maps a goroutine's g pointer to its own JNIEnv slot: a single
// uintptr holding the interface-table base. Entries live as long as the guest
// thread does; releaseEnvSlot clears one when the goroutine exits so the reused
// g struct cannot inherit a stale env.
var (
	envSlotMu  sync.Mutex
	envSlotByG = map[uintptr]*uintptr{}
)

func envSlotForCurrentThread() uintptr {
	g := getg()
	envSlotMu.Lock()
	p := envSlotByG[g]
	if p == nil {
		p = new(uintptr)
		*p = jniEnvSlot
		envSlotByG[g] = p
	}
	envSlotMu.Unlock()
	return uintptr(unsafe.Pointer(p))
}

func releaseEnvSlot(g uintptr) {
	envSlotMu.Lock()
	delete(envSlotByG, g)
	envSlotMu.Unlock()
}

func jniInit() {
	jniEnvTable = make([]uintptr, jniEnvTableLen)
	jniVMTable = make([]uintptr, jniVMTableLen)
	for i := range jniEnvTable {
		jniEnvTable[i] = mkJNIThunk(uintptr(i))
	}
	for i := range jniVMTable {
		jniVMTable[i] = mkJNIThunk(uintptr(jniVMIDBase + i))
	}
	jniEnvSlot = uintptr(unsafe.Pointer(&jniEnvTable[0]))
	jniVMSlot = uintptr(unsafe.Pointer(&jniVMTable[0]))
	jniEnvAddr = uintptr(unsafe.Pointer(&jniEnvSlot))
	jniVMAddr = uintptr(unsafe.Pointer(&jniVMSlot))
	arenaMakeExec()
}

// mkJNIThunk emits a trampoline that loads id into R10 and jumps to jniEntry
// through an inline absolute slot. R10 is scratch under the SysV ABI, so
// clobbering it here is invisible to the guest; the callee-saved registers are
// preserved by jniEntry itself.
func mkJNIThunk(id uintptr) uintptr {
	b := codeArena(32)
	off := 0
	// MOVL $id, R10d  (41 BA imm32)
	b[off] = 0x41
	b[off+1] = 0xBA
	put32(b[off+2:], uint32(id))
	off += 6
	// JMP [RIP+0] followed by the 8-byte absolute target.
	copy(b[off:], []byte{0xFF, 0x25, 0x00, 0x00, 0x00, 0x00})
	off += 6
	put64(b[off:], jniEntryAddr())
	off += 8
	return uintptr(unsafe.Pointer(&b[0]))
}

// ld64 reads a word from guest memory.
func ld64(p uintptr) uintptr { return *(*uintptr)(unsafe.Pointer(p)) }

// stPtr writes a pointer-sized value to guest memory.
func stPtr(dst, v uintptr) { st64(dst, uint64(v)) }

// --- handle bookkeeping --------------------------------------------------

var (
	jniMu           sync.Mutex
	jniClasses              = map[string]uintptr{}
	jniClassNext    uintptr = 0x10000
	jniMethodKeys           = map[string]uintptr{}
	jniMethodNext   uintptr = 0x1000000
	jniFieldKeys            = map[string]uintptr{}
	jniFieldNext    uintptr = 0x1800000
	jniStrings              = map[uintptr][]byte{}
	jniStringNext   uintptr = 0x2000000
	jniUnimplSeen           = map[uintptr]bool{}
	jniNatives              = map[uintptr]map[string]uintptr{}
	jniLoggedNative         = 0
	jniCallLog      atomic.Int32
)

func jniFindClassHandle(name string) uintptr {
	jniMu.Lock()
	defer jniMu.Unlock()
	if h, ok := jniClasses[name]; ok {
		return h
	}
	h := jniClassNext
	jniClassNext += 16
	jniClasses[name] = h
	jniNoteClass(h, name)
	fmt.Fprintf(os.Stderr, "rbxrt: JNI: FindClass %q -> %#x\n", name, h)
	return h
}

// FindClassHandle returns the JNI class handle for name, allocating it if the
// engine has not looked the class up itself. When the launch driver has to
// stand in for Java and call a static native directly, it needs the same
// jclass the engine would have passed.
func FindClassHandle(name string) uintptr { return jniFindClassHandle(name) }

func jniMethodHandle(cls uintptr, name, sig string) uintptr {
	key := fmt.Sprintf("%x\x00%s\x00%s", cls, name, sig)
	jniMu.Lock()
	if h, ok := jniMethodKeys[key]; ok {
		jniMu.Unlock()
		return h
	}
	h := jniMethodNext
	jniMethodNext += 16
	jniMethodKeys[key] = h
	jniMu.Unlock()
	m := jniMember{jniClassName(cls), name, sig}
	jniObjMu.Lock()
	jniMethodIds[h] = m
	jniObjMu.Unlock()
	jniTraceMember("GetMethodID", m)
	return h
}

func jniFieldHandle(cls uintptr, name, sig string) uintptr {
	key := fmt.Sprintf("%x\x00%s\x00%s", cls, name, sig)
	jniMu.Lock()
	if h, ok := jniFieldKeys[key]; ok {
		jniMu.Unlock()
		return h
	}
	h := jniFieldNext
	jniFieldNext += 16
	jniFieldKeys[key] = h
	jniMu.Unlock()
	m := jniMember{jniClassName(cls), name, sig}
	jniObjMu.Lock()
	jniFieldIds[h] = m
	jniObjMu.Unlock()
	jniTraceMember("GetFieldID", m)
	return h
}

func jniNewStringFromUTF(s string) uintptr {
	b := make([]byte, len(s)+1)
	copy(b, s)
	jniMu.Lock()
	h := jniStringNext
	jniStringNext += 16
	jniStrings[h] = b
	jniMu.Unlock()
	return h
}

// jniNewStringFromUTF16 converts a jchar array to a byte slice (BMP only, which
// is all the runtime needs today).
func jniNewStringFromUTF16(p uintptr, n int) uintptr {
	b := make([]byte, 0, n+1)
	for i := 0; i < n; i++ {
		u := *(*uint16)(unsafe.Pointer(p + uintptr(i)*2))
		if u < 0x80 {
			b = append(b, byte(u))
		} else {
			b = append(b, '?')
		}
	}
	b = append(b, 0)
	jniMu.Lock()
	h := jniStringNext
	jniStringNext += 16
	jniStrings[h] = b
	jniMu.Unlock()
	return h
}

func jniStringBytes(h uintptr) uintptr {
	jniMu.Lock()
	defer jniMu.Unlock()
	b := jniStrings[h]
	if b == nil {
		b = []byte{0}
		jniStrings[h] = b
	}
	return uintptr(unsafe.Pointer(&b[0]))
}

func jniStringLen(h uintptr) int {
	jniMu.Lock()
	defer jniMu.Unlock()
	return len(jniStrings[h])
}

// jniRecordNatives walks a JNINativeMethod[] ({name, signature, fnPtr}, 24
// bytes each) and records each guest implementation so the runtime can call it
// later when it plays the role of Java.
func jniRecordNatives(cls, methods uintptr, n int) {
	jniMu.Lock()
	m, ok := jniNatives[cls]
	if !ok {
		m = map[string]uintptr{}
		jniNatives[cls] = m
	}
	jniLoggedNative += n
	dir := len(jniNatives)
	jniMu.Unlock()
	for i := 0; i < n; i++ {
		base := methods + uintptr(i)*24
		name := cstr(ld64(base))
		sig := cstr(ld64(base + 8))
		fn := ld64(base + 16)
		m[name] = fn
		if debugJNI {
			fmt.Fprintf(os.Stderr, "rbxrt: JNI:   native %q %q -> image+%#x\n", name, sig, fn)
		}
	}
	fmt.Fprintf(os.Stderr, "rbxrt: JNI: RegisterNatives class=%#x count=%d (classes=%d, total natives=%d)\n",
		cls, n, dir, jniLoggedNative)
}

func jniUnimpl(id uintptr) uintptr {
	jniMu.Lock()
	seen := jniUnimplSeen[id]
	jniUnimplSeen[id] = true
	jniMu.Unlock()
	if !seen {
		fmt.Fprintf(os.Stderr, "rbxrt: JNI: unimplemented JNIEnv call (index %d)\n", id)
	}
	return 0
}

// NativeMethod returns the guest function registered for a native method on the
// named class, e.g. NativeMethod("com/google/androidgamesdk/GameActivity",
// "onStartNative"). It lets the runtime play the role of Java and drive the
// lifecycle the framework would.
func NativeMethod(class, name string) (uintptr, bool) {
	jniMu.Lock()
	defer jniMu.Unlock()
	h, ok := jniClasses[class]
	if !ok {
		return 0, false
	}
	fn, ok := jniNatives[h][name]
	return fn, ok
}

// NativeNames returns the registered native method names for a class, sorted.
func NativeNames(class string) []string {
	jniMu.Lock()
	defer jniMu.Unlock()
	m := jniNatives[jniClasses[class]]
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// --- dispatch ------------------------------------------------------------

// goJNIEntry is the single dispatcher behind every JNI table entry. It is the
// target of the jniEntry trampoline (see abi_amd64.s); its ABIInternal entry is
// published through jniTarget.
//
//go:noinline
func goJNIEntry(id, args uintptr) uintptr {
	noteSP("jni")
	if debugJNI && jniCallLog.Add(1) <= 300 {
		fmt.Fprintf(os.Stderr, "rbxrt: JNI call id=%d\n", id)
	}
	a0 := ld64(args)
	if id >= jniVMIDBase {
		return jniVMFn(id-jniVMIDBase, a0, ld64(args+8), ld64(args+16))
	}
	return jniEnvFn(id, a0, ld64(args+8), ld64(args+16), ld64(args+24), ld64(args+32), ld64(args+40))
}

func jniVMFn(idx, vm, a1, a2 uintptr) uintptr {
	switch idx {
	case jniGetEnv, jniAttachCurrentThread, jniAttachCurrentThreadAsDaemon:
		if a1 != 0 {
			stPtr(a1, envSlotForCurrentThread())
		}
		return 0
	case jniDetachCurrentThread, jniDestroyJavaVM:
		return 0
	}
	return 0
}

func jniEnvFn(id, env, a1, a2, a3, a4, a5 uintptr) uintptr {
	// Ranges with real behaviour in the object model (jobject.go): instance
	// calls and field reads are answered from the value tables the driver
	// fills, writes are stored so a later read sees them.
	switch {
	case id >= 34 && id <= 63: // Call<Type>Method[V/A]
		return jniCallInstance(id, a1, a2, jniFirstCallArg(id, a3))
	case id >= 95 && id <= 112: // Get<Type>Field 95..103, Set<Type>Field 104..112
		// The JNI table lists the nine getters (Object, Boolean, Byte, Char,
		// Short, Int, Long, Float, Double) and then the nine setters. An
		// earlier "odd = get" rule answered every Boolean/Char/Int/Float
		// field read as a write, so they all read 0 - the engine saw a
		// PlatformParams.dpiScale of 0 and laid its whole UI out at zero size.
		if id <= 103 {
			return jniGetFieldValue(a1, a2)
		}
		jniSetFieldValue(a1, a2, a3)
		return 0
	case id >= 114 && id <= 143: // CallStatic<Type>Method[V/A]
		return jniCallStatic(a1, a2, jniFirstCallArg(id, a3))
	case id >= 145 && id <= 162: // GetStatic<Type>Field 145..153, SetStatic 154..162
		if id <= 153 {
			return jniGetStaticFieldValue(a1, a2)
		}
		jniSetStaticFieldValue(a1, a2, a3)
		return 0
	case id >= 171 && id <= 214: // array access and creation
		// NOTE: the range must start at 171 (GetArrayLength). The string
		// functions live at 163..170 and have real handlers below; an earlier
		// 164 lower bound shadowed them, so GetStringUTFChars (169) silently
		// returned NULL and the guest's std::string assignment crashed on it.
		return 0
	}

	switch id {
	case jniGetVersion:
		return jniVersion16
	case jniFindClass:
		return jniFindClassHandle(cstr(a1))
	case jniExceptionOccurred, jniExceptionClear, jniExceptionCheck:
		return 0
	case jniNewGlobalRef, jniNewLocalRef, jniNewWeakGlobalRef:
		return a1
	case jniDeleteGlobalRef, jniDeleteLocalRef, jniDeleteWeakGlobalRef:
		return 0
	case jniIsSameObject:
		if a1 == a2 {
			return 1
		}
		return 0
	case jniGetObjectClass:
		// A fake object resolves to its class; a class handle is already one.
		if name := jniObjectClass(a1); name != "" {
			return jniFindClassHandle(name)
		}
		return a1
	case jniAllocObject, jniNewObject, jniNewObjectV, jniNewObjectA:
		// The engine constructs Java objects of its own (bundles, params):
		// mint one of the requested class so its field reads resolve.
		return NewFakeObject(jniClassName(a1))
	case jniIsInstanceOf:
		return 1
	case jniGetMethodID, jniGetStaticMethodID:
		return jniMethodHandle(a1, cstr(a2), cstr(a3))
	case jniGetFieldID, jniGetStaticFieldID:
		return jniFieldHandle(a1, cstr(a2), cstr(a3))
	case jniNewStringUTF:
		return jniNewStringFromUTF(cstr(a1))
	case jniNewString:
		return jniNewStringFromUTF16(a1, int(a2))
	case jniGetStringUTFLength:
		return uintptr(jniStringLen(a1))
	case jniGetStringUTFChars:
		if a2 != 0 {
			st8(a2, 1) // JNI_TRUE: the guest must call ReleaseStringUTFChars
		}
		return jniStringBytes(a1)
	case jniReleaseStringUTFChars:
		return 0
	case jniGetArrayLength:
		return 0
	case jniRegisterNatives:
		jniRecordNatives(a1, a2, int(a3))
		return 0
	case jniUnregisterNatives:
		return 0
	case jniMonitorEnter, jniMonitorExit:
		return 0
	case jniGetJavaVM:
		if a1 != 0 {
			stPtr(a1, jniVMAddr)
		}
		return 0
	case jniNewDirectByteBuffer, jniGetDirectBufferAddress, jniGetDirectBufferCapacity:
		return 0
	case jniGetObjectRefType:
		return 1 // JNILocalRefType
	}
	return jniUnimpl(id)
}

// jniFirstCallArg normalizes the first argument of the three JNI call forms.
// Direct calls pass it in the third register; V calls pass a SysV va_list;
// A calls pass a jobject array. This matters for ClassLoader.findClass, whose
// argument is otherwise mistaken for the va_list address itself.
func jniFirstCallArg(id, p uintptr) uintptr {
	if p == 0 {
		return 0
	}
	switch id % 3 {
	case 2: // Call<Type>MethodV
		gp := uint32(ld64(p))
		if gp < 48 {
			return ld64(ld64(p+16) + uintptr(gp))
		}
		return ld64(ld64(p + 8))
	case 0: // Call<Type>MethodA
		return ld64(p)
	default: // direct varargs form
		return p
	}
}
