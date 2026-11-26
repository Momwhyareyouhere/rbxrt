package hostabi

import (
	"strings"
	"sync"
)

// Java language behaviour the engine relies on through JNI and that is the
// same for every class, so it is answered generically rather than per class:
//
//   - Enums. Generated bridges (djinni) turn a C++ enum into its Java constant
//     with X.values()[ordinal] and back with x.ordinal(). values() returns a
//     real Object[] of distinct constants (one array per enum class, so the
//     same constant is the same object every time) and ordinal() answers each
//     constant's index. The array is sized generously: the engine only indexes
//     it with ordinals it knows.
//   - Boxed primitives. Boolean/Byte/Short/Character/Integer/Long.valueOf box
//     an integer value that xxxValue() and equals-style reads give back.
//     Float/Double.valueOf arrive in a vector register the JNI entry does not
//     save, so they box 0.
//
// A value the driver registered explicitly (SetFakeMethodResult/Func) always
// takes precedence; these are the fallbacks for calls nobody answered.

const enumArraySize = 64

var jlang = struct {
	sync.Mutex
	enumValues map[string]uintptr  // enum class -> its values() array
	ordinals   map[uintptr]uintptr // enum constant -> ordinal
	boxed      map[uintptr]uintptr // boxed object -> primitive value
}{enumValues: map[string]uintptr{}, ordinals: map[uintptr]uintptr{}, boxed: map[uintptr]uintptr{}}

var boxClasses = map[string]bool{
	"java/lang/Boolean": true, "java/lang/Byte": true, "java/lang/Short": true,
	"java/lang/Character": true, "java/lang/Integer": true, "java/lang/Long": true,
	"java/lang/Float": true, "java/lang/Double": true,
}

// jlangStatic answers X.values() for an enum and Box.valueOf(v).
func jlangStatic(class string, m jniMember, arg uintptr) (uintptr, bool) {
	switch {
	case m.name == "values" && strings.HasPrefix(m.sig, "()[L"):
		return enumValuesArray(class), true
	case m.name == "valueOf" && boxClasses[class]:
		obj := NewFakeObject(class)
		jlang.Lock()
		if !strings.Contains(m.sig, "(F)") && !strings.Contains(m.sig, "(D)") {
			jlang.boxed[obj] = arg
		}
		jlang.Unlock()
		return obj, true
	}
	return 0, false
}

func enumValuesArray(class string) uintptr {
	jlang.Lock()
	if a, ok := jlang.enumValues[class]; ok {
		jlang.Unlock()
		return a
	}
	jlang.Unlock()
	consts := make([]uintptr, enumArraySize)
	for i := range consts {
		consts[i] = NewFakeObject(class)
	}
	a := newObjectArray(enumArraySize, 0)
	jniArrays.Lock()
	copy(jniArrays.objs[a], consts)
	jniArrays.Unlock()
	jlang.Lock()
	defer jlang.Unlock()
	if prev, ok := jlang.enumValues[class]; ok { // lost a race: keep the first
		return prev
	}
	for i, c := range consts {
		jlang.ordinals[c] = uintptr(i)
	}
	jlang.enumValues[class] = a
	return a
}

// jlangInstance answers x.ordinal() on an enum constant and the unboxing
// getters on a boxed primitive.
func jlangInstance(obj uintptr, m jniMember) (uintptr, bool) {
	jlang.Lock()
	defer jlang.Unlock()
	if m.name == "ordinal" {
		v, ok := jlang.ordinals[obj]
		return v, ok
	}
	switch m.name {
	case "booleanValue", "byteValue", "shortValue", "charValue", "intValue", "longValue":
		v, ok := jlang.boxed[obj]
		return v, ok
	}
	return 0, false
}
