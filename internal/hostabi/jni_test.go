package hostabi

import (
	"math"
	"testing"
	"unsafe"
)

// jniFloatStub calls fn(env, obj, fid) and returns the low 32 bits of XMM0,
// i.e. what a guest reading a jfloat result sees:
//
//	jnif(fn=rdi, env=rsi, obj=rdx, fid=rcx):
//		push %rbp; mov %rsp,%rbp
//		mov %rdi,%rax; mov %rsi,%rdi; mov %rdx,%rsi; mov %rcx,%rdx
//		xorps %xmm0,%xmm0; call *%rax
//		movd %xmm0,%eax; pop %rbp; ret
var jniFloatStub = []byte{
	0x55, 0x48, 0x89, 0xe5, 0x48, 0x89, 0xf8, 0x48, 0x89, 0xf7, 0x48, 0x89,
	0xd6, 0x48, 0x89, 0xca, 0x0f, 0x57, 0xc0, 0xff, 0xd0, 0x66, 0x0f, 0x7e,
	0xc0, 0x5d, 0xc3,
}

// The engine reads PlatformParams through the JNI table exactly like this:
// GetIntField (100) must return the stored int and GetFloatField (102) the
// stored float in XMM0. Both used to read 0, which laid the UI out at zero size.
func TestJNIFieldGettersByType(t *testing.T) {
	const class = "rbxrt/test/Params"
	obj := NewFakeObject(class)
	SetFakeField(class, "count", 7)
	SetFakeField(class, "flag", 1)
	SetFakeField(class, "scale", uintptr(math.Float32bits(1.5)))
	cls := FindClassHandle(class)

	env := JNIEnvAddr()
	table := *(*uintptr)(unsafe.Pointer(env))
	fn := func(idx uintptr) uintptr { return *(*uintptr)(unsafe.Pointer(table + idx*8)) }
	fid := func(name, sig string) uintptr { return jniFieldHandle(cls, name, sig) }

	if got := CallGuestInt3(fn(100), env, obj, fid("count", "I")); got != 7 {
		t.Fatalf("GetIntField = %d, want 7", got)
	}
	if got := CallGuestInt3(fn(96), env, obj, fid("flag", "Z")); got&0xff != 1 {
		t.Fatalf("GetBooleanField = %d, want 1", got)
	}
	stub := codeArena(len(jniFloatStub))
	copy(stub, jniFloatStub)
	arenaMakeExec()
	bits := uint32(CallGuestInt8(uintptr(unsafe.Pointer(&stub[0])), fn(102), env, obj, fid("scale", "F"), 0, 0, 0, 0))
	if got := math.Float32frombits(bits); got != 1.5 {
		t.Fatalf("GetFloatField (XMM0) = %v, want 1.5", got)
	}
	// A setter must not be mistaken for a getter: SetIntField (109) stores.
	CallGuestInt8(fn(109), env, obj, fid("count", "I"), 9, 0, 0, 0, 0)
	if got := CallGuestInt3(fn(100), env, obj, fid("count", "I")); got != 9 {
		t.Fatalf("GetIntField after SetIntField = %d, want 9", got)
	}
}
