package hostabi

import (
	"fmt"
	"os"
	"strconv"
	"sync"
)

// The engine receives its Java-side data through JNI: it looks classes up,
// asks for field and method ids by name, and reads values back through them.
// There is no ART behind those calls here, so the runtime models them
// explicitly instead of leaving them to return zero:
//
//   - an object is a handle that remembers which class it instantiates, so
//     GetObjectClass and instance reads resolve to that class;
//   - a field or method id remembers the (class, name, signature) it was
//     minted for, so a request can be reported in the terms the engine used;
//   - values live in a per-(class, member) table the launch driver fills in,
//     one entry at a time, as the engine's requests become known.
//
// A read with no entry reports itself once, by name, and returns zero - the
// same loud contract as an unimplemented host symbol, because a silently null
// field is how a startup path disappears without leaving a trace.

// jniMember is what a field or method id denotes.
type jniMember struct {
	class string
	name  string
	sig   string
}

var (
	jniObjMu         sync.Mutex
	jniObjNext       uintptr = 0x30000000
	jniObjClasses            = map[uintptr]string{}    // object handle -> class
	jniClassNames            = map[uintptr]string{}    // class handle -> class
	jniFieldIds              = map[uintptr]jniMember{} // field id -> member
	jniMethodIds             = map[uintptr]jniMember{} // method id -> member
	jniFieldValues           = map[[2]string]uintptr{} // (class, name) -> value
	jniMethodResults         = map[[2]string]uintptr{} // (class, name) -> result
	jniMethodFuncs           = map[[2]string]func() uintptr{}
	jniMethodArgFuncs        = map[[2]string]func(arg uintptr) uintptr{}
	jniReported              = map[string]bool{}
	jniNativeGLImplementation uintptr
)

// NewFakeObject mints a handle for an instance of class, which the driver
// passes to the engine where the Java app would have passed a real object.
func NewFakeObject(class string) uintptr {
	jniObjMu.Lock()
	defer jniObjMu.Unlock()
	h := jniObjNext
	jniObjNext += 16
	jniObjClasses[h] = class
	return h
}

// NewJavaString mints a jstring handle the engine can read with the string
// JNI functions, for fields and method results that are strings.
func NewJavaString(s string) uintptr { return jniNewStringFromUTF(s) }

// SetJavaString defines the bytes a string handle stands for. The launch passes
// fixed sentinels where the framework would pass real Strings (the asset path,
// the native library directory, the data directory); without this they read as
// empty, which is a silently wrong value rather than a missing one.
func SetJavaString(h uintptr, s string) {
	b := make([]byte, len(s)+1)
	copy(b, s)
	jniMu.Lock()
	jniStrings[h] = b
	jniMu.Unlock()
}

// JavaStringValue returns the text of a Java string handle (one the runtime
// or the engine created with NewStringUTF), and false if h is not a string.
func JavaStringValue(h uintptr) (string, bool) {
	jniMu.Lock()
	defer jniMu.Unlock()
	b, ok := jniStrings[h]
	if !ok {
		return "", false
	}
	if n := len(b); n > 0 && b[n-1] == 0 {
		b = b[:n-1]
	}
	return string(b), true
}

// SetFakeField gives a field of a fake class the value the engine reads back.
// An object field takes a handle (NewJavaString or NewFakeObject), a numeric
// field the number itself.
func SetFakeField(class, field string, v uintptr) {
	jniObjMu.Lock()
	defer jniObjMu.Unlock()
	jniFieldValues[[2]string{class, field}] = v
}

// SetFakeMethodResult gives a method of a fake class the result the engine
// gets back from calling it.
func SetFakeMethodResult(class, method string, v uintptr) {
	jniObjMu.Lock()
	defer jniObjMu.Unlock()
	jniMethodResults[[2]string{class, method}] = v
}

// SetFakeMethodFunc answers a method with a value computed at each call, for
// members whose answer is not fixed (a clock, a counter). It takes precedence
// over SetFakeMethodResult.
func SetFakeMethodFunc(class, method string, f func() uintptr) {
	jniObjMu.Lock()
	defer jniObjMu.Unlock()
	jniMethodFuncs[[2]string{class, method}] = f
}

// SetFakeMethodArgFunc answers a method, instance or static, with a value
// computed from its first argument (e.g. NativeGLJavaInterface.showKeyboard
// receives the text box it is shown for). It takes precedence over
// SetFakeMethodFunc and SetFakeMethodResult.
func SetFakeMethodArgFunc(class, method string, f func(arg uintptr) uintptr) {
	jniObjMu.Lock()
	defer jniObjMu.Unlock()
	jniMethodArgFuncs[[2]string{class, method}] = f
}

// jniMethodArgValue runs the argument-taking answer for (class, method), if any.
func jniMethodArgValue(cls, name string, arg uintptr) (uintptr, bool) {
	jniObjMu.Lock()
	f, ok := jniMethodArgFuncs[[2]string{cls, name}]
	jniObjMu.Unlock()
	if !ok {
		return 0, false
	}
	return f(arg), true
}

// SetNativeGLImplementation models NativeGLJavaInterface.setImplementation,
// which is a Java-side singleton assignment performed by ActivityNativeMain
// before the app bridge starts.
func SetNativeGLImplementation(v uintptr) { jniNativeGLImplementation = v }

// jniMethodValue resolves a (class, method) answer: a computed one when the
// driver registered a function, otherwise the stored value.
func jniMethodValue(cls, name string) (uintptr, bool) {
	jniObjMu.Lock()
	defer jniObjMu.Unlock()
	if f, ok := jniMethodFuncs[[2]string{cls, name}]; ok {
		return f(), true
	}
	v, ok := jniMethodResults[[2]string{cls, name}]
	return v, ok
}

// jniNoteClass records the name a class handle stands for.
func jniNoteClass(h uintptr, name string) {
	jniObjMu.Lock()
	defer jniObjMu.Unlock()
	jniClassNames[h] = name
}

// NameJavaObject tells the runtime what class a sentinel handle (one the launch
// passes in place of a real Java object, such as the activity) stands for, so
// calls the engine makes on it resolve and report by name.
func NameJavaObject(h uintptr, class string) {
	jniObjMu.Lock()
	defer jniObjMu.Unlock()
	jniObjClasses[h] = class
}

// jniClassName names a class handle, "" when it is not one.
func jniClassName(h uintptr) string {
	jniObjMu.Lock()
	defer jniObjMu.Unlock()
	return jniClassNames[h]
}

// jniObjectClass names the class of an object or class handle, "" for
// handles the runtime did not mint.
func jniObjectClass(obj uintptr) string {
	jniObjMu.Lock()
	defer jniObjMu.Unlock()
	if c, ok := jniObjClasses[obj]; ok {
		return c
	}
	return jniClassNames[obj]
}

// jniReport notes a member read the driver has not answered yet, once per
// member, so a run's log lists exactly what still needs a value.
func jniReport(kind string, m jniMember) {
	key := kind + " " + m.class + "." + m.name
	jniObjMu.Lock()
	if jniReported[key] {
		jniObjMu.Unlock()
		return
	}
	jniReported[key] = true
	jniObjMu.Unlock()
	fmt.Fprintf(os.Stderr, "rbxrt: JNI: %s %s.%s%s has no value (returns 0)\n",
		kind, m.class, m.name, m.sig)
}

// jniCallInstance handles the Call<Type>Method range: obj has the method id,
// and the result comes from the class the object instantiates.
func jniCallInstance(_ uintptr, obj, mid, arg uintptr) uintptr {
	m := jniMethodIds[mid]
	cls := jniObjectClass(obj)
	// MainGameActivity's init-params path asks its ClassLoader to resolve
	// native model classes. A real CallObjectMethod carries the requested
	// class name as its first argument; mirror ART by returning that class
	// handle instead of the null placeholder.
	if m.name == "findClass" || m.name == "loadClass" {
		if arg != 0 {
			return jniFindClassHandle(cstr(arg))
		}
	}
	// String.getBytes(charset): the engine's jstring -> std::string path.
	if m.name == "getBytes" {
		if v, ok := jniStringGetBytes(obj); ok {
			return v
		}
	}
	if v, ok := jniMethodArgValue(cls, m.name, arg); ok {
		jniTraceCall("method", cls, m.name, v)
		return v
	}
	if v, ok := jniMethodValue(cls, m.name); ok {
		jniTraceCall("method", cls, m.name, v)
		return v
	}
	// The engine sometimes mints a method id against one class and invokes it on
	// an object the runtime named differently; the id's class is the fallback.
	if cls != m.class {
		if v, ok := jniMethodValue(m.class, m.name); ok {
			jniTraceCall("method", m.class, m.name, v)
			return v
		}
	}
	if v, ok := jlangInstance(obj, m); ok {
		jniTraceCall("method", cls, m.name, v)
		return v
	}
	jniReport("method call", jniMember{cls, m.name, m.sig})
	return 0
}

// jniCallStatic handles CallStatic<Type>Method: the class is the first
// argument, and the result is keyed by that class.
func jniCallStatic(cls, mid, arg uintptr) uintptr {
	m := jniMethodIds[mid]
	name := jniClassName(cls)
	if name == "" {
		name = m.class
	}
	if name == "com/roblox/engine/jni/NativeGLJavaInterface" && m.name == "getImplementation" {
		return jniNativeGLImplementation
	}
	if name == "com/roblox/engine/jni/NativeGLJavaInterface" && m.name == "setImplementation" {
		jniNativeGLImplementation = arg
		return 0
	}
	if v, ok := jniMethodArgValue(name, m.name, arg); ok {
		jniTraceCall("static", name, m.name, v)
		return v
	}
	if v, ok := jniMethodValue(name, m.name); ok {
		jniTraceCall("static", name, m.name, v)
		return v
	}
	if v, ok := jlangStatic(name, m, arg); ok {
		jniTraceCall("static", name, m.name, v)
		return v
	}
	jniReport("static call", jniMember{name, m.name, m.sig})
	return 0
}

// jniGetFieldValue handles the Get<Type>Field range.
func jniGetFieldValue(obj, fid uintptr) uintptr {
	m := jniFieldIds[fid]
	cls := jniObjectClass(obj)
	jniObjMu.Lock()
	v, ok := jniFieldValues[[2]string{cls, m.name}]
	jniObjMu.Unlock()
	if !ok && m.class != cls {
		jniObjMu.Lock()
		v, ok = jniFieldValues[[2]string{m.class, m.name}]
		jniObjMu.Unlock()
	}
	if !ok {
		jniReport("field read", jniMember{cls, m.name, m.sig})
		return 0
	}
	jniTraceCall("field", cls, m.name, v)
	return v
}

// jniSetFieldValue handles the Set<Type>Field range: a write is stored so a
// later read of the same field sees it.
func jniSetFieldValue(obj, fid, v uintptr) {
	m := jniFieldIds[fid]
	cls := jniObjectClass(obj)
	jniObjMu.Lock()
	defer jniObjMu.Unlock()
	jniFieldValues[[2]string{cls, m.name}] = v
}

// jniGetStaticFieldValue handles GetStatic<Type>Field.
func jniGetStaticFieldValue(cls, fid uintptr) uintptr {
	m := jniFieldIds[fid]
	name := jniClassName(cls)
	if name == "" {
		name = m.class
	}
	jniObjMu.Lock()
	v, ok := jniFieldValues[[2]string{name, m.name}]
	jniObjMu.Unlock()
	if !ok {
		jniReport("static field read", jniMember{name, m.name, m.sig})
		return 0
	}
	jniTraceCall("static field", name, m.name, v)
	return v
}

// jniSetStaticFieldValue handles SetStatic<Type>Field.
func jniSetStaticFieldValue(cls, fid, v uintptr) {
	m := jniFieldIds[fid]
	name := jniClassName(cls)
	if name == "" {
		name = m.class
	}
	jniObjMu.Lock()
	defer jniObjMu.Unlock()
	jniFieldValues[[2]string{name, m.name}] = v
}

// jniTraceLimit is how many trace lines the JNI trace prints before it goes
// quiet; the engine's startup asks for more members than a frame ever will, so
// the default keeps a normal run's log short and RBXRT_JNI_TRACE_LIMIT raises
// it when the interesting part is later.
func jniTraceLimit() int64 {
	if v := os.Getenv("RBXRT_JNI_TRACE_LIMIT"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return 300
}

// jniTraceMember logs a minted id when tracing is on, so the log shows what
// the engine asked for before it asked for the value.
func jniTraceMember(kind string, m jniMember) {
	if !debugJNI {
		return
	}
	if int64(jniCallLog.Add(1)) > jniTraceLimit() {
		return
	}
	fmt.Fprintf(os.Stderr, "rbxrt: JNI: %s %s.%s%s\n", kind, m.class, m.name, m.sig)
}

// jniTraceCall logs the answer a read was given. With the member trace above it
// shows the engine's JNI conversation up to the last call it made, which is
// where a crash inside guest code leaves off.
func jniTraceCall(kind, cls, name string, v uintptr) {
	if !debugJNI {
		return
	}
	if int64(jniCallLog.Add(1)) > jniTraceLimit() {
		return
	}
	fmt.Fprintf(os.Stderr, "rbxrt: JNI: -> %s %s.%s = %#x\n", kind, cls, name, v)
}
