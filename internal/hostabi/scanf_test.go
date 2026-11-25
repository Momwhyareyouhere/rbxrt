package hostabi

import (
	"testing"
	"unsafe"
)

// The GLES renderer parses its uniform blocks out of every shader with
// sscanf(line, "uniform vec4 CB%d [%d];", &slot, &count); the format's space
// must match zero characters, since the shaders have none before '['.
func TestSscanfUniformDeclaration(t *testing.T) {
	format, unmapF := putCString(t, "uniform vec4 CB%d [%d];")
	defer unmapF()
	for _, line := range []string{"uniform vec4 CB1[58];", "uniform vec4 CB0 [12];"} {
		str, unmapS := putCString(t, line)
		out, unmapO := pin(t)
		n := goVsscanf(str, format, newVaList(t, out, out+8))
		a := *(*int32)(unsafe.Pointer(out))
		b := *(*int32)(unsafe.Pointer(out + 8))
		unmapS()
		unmapO()
		if n != 2 || (a != 1 && a != 0) || (b != 58 && b != 12) {
			t.Errorf("sscanf(%q) = %d (a=%d b=%d), want 2 with the slot and count", line, n, a, b)
		}
	}
}

func TestSscanfIntegersStopAtNonDigits(t *testing.T) {
	cases := []struct {
		in, format string
		want       []int32
	}{
		{"uniform vec4 CB1[58];", "uniform vec4 CB%d [%d];", []int32{1, 58}},
		{"12,34", "%d,%d", []int32{12, 34}},
		{"0x1fg 017 9", "%x%*c %o %d", []int32{0x1f, 017, 9}},
		{"-7x", "%dx", []int32{-7}},
		{"123456", "%3d%d", []int32{123, 456}},
	}
	for _, c := range cases {
		str, unmapS := putCString(t, c.in)
		format, unmapF := putCString(t, c.format)
		out, unmapO := pin(t)
		args := make([]uintptr, len(c.want))
		for i := range args {
			args[i] = out + uintptr(i)*8
		}
		n := goVsscanf(str, format, newVaList(t, args...))
		for i, w := range c.want {
			if got := *(*int32)(unsafe.Pointer(out + uintptr(i)*8)); got != w {
				t.Errorf("sscanf(%q, %q) arg %d = %d, want %d", c.in, c.format, i, got, w)
			}
		}
		if n != len(c.want) {
			t.Errorf("sscanf(%q, %q) = %d, want %d", c.in, c.format, n, len(c.want))
		}
		unmapS()
		unmapF()
		unmapO()
	}
}

func TestSscanfFloats(t *testing.T) {
	str, unmapS := putCString(t, " 1.5,-2e3")
	defer unmapS()
	format, unmapF := putCString(t, "%f,%lf")
	defer unmapF()
	out, unmapO := pin(t)
	defer unmapO()
	n := goVsscanf(str, format, newVaList(t, out, out+8))
	f := *(*float32)(unsafe.Pointer(out))
	d := *(*float64)(unsafe.Pointer(out + 8))
	if n != 2 || f != 1.5 || d != -2000 {
		t.Fatalf("sscanf floats = %d (%v, %v), want 2 (1.5, -2000)", n, f, d)
	}
}
