package hostabi

import (
	"fmt"
	"strings"
	"sync/atomic"
	"unsafe"
)

// Mobile GLES drivers (Adreno, Mali) accept GLSL ES 3.00 that mixes integer
// and float/unsigned operands without casts, and the engine's shader pack
// relies on it: after it rewrites uniform-array indexing, terrain shaders
// contain `CB12[(uint(x) & 255u) * 1 + 0]` - a uint * int multiply that the
// GLSL ES 3.00 specification (and Mesa, which follows it) rejects. Mesa does
// implement GL_EXT_shader_implicit_conversions, which permits exactly these
// conversions (int -> uint, int/uint -> float) but is defined for GLSL ES 3.10.
//
// glShaderSource is therefore wrapped: a "#version 300 es" shader is
// resubmitted as "#version 310 es" with that extension enabled. ES 3.10 is a
// superset of 3.00 for the constructs these shaders use, the context is
// OpenGL ES 3.2, and the rewrite is validated against the full shader pack by
// TestGLSLRelaxCorpus. Everything else passes through unchanged.

const (
	glslES300 = "#version 300 es"
	glslES310 = "#version 310 es\n#extension GL_EXT_shader_implicit_conversions : enable"
)

var (
	glShaderSourceHost uintptr
	glslRelaxed        atomic.Int64
)

// relaxGLSL returns src with the GLSL ES 3.00 version directive replaced as
// described above, or src unchanged when it is not an ES 3.00 shader. The
// directive must be the first token, so only a leading one is rewritten.
func relaxGLSL(src string) (string, bool) {
	trimmed := strings.TrimLeft(src, " \t\r\n")
	if !strings.HasPrefix(trimmed, glslES300) {
		return src, false
	}
	rest := trimmed[len(glslES300):]
	if rest != "" && rest[0] != '\n' && rest[0] != '\r' && rest[0] != ' ' && rest[0] != '\t' {
		return src, false // e.g. "#version 300 esx": not ours to touch
	}
	return glslES310 + rest, true
}

// goGlShaderSource is glShaderSource(shader, count, strings, lengths). The
// sources are joined (a negative or absent length means NUL-terminated),
// relaxed, and handed to the host as one NUL-terminated string.
//
//go:noinline
func goGlShaderSource(shader, count, strs, lens uintptr) uintptr {
	var b strings.Builder
	for i := uintptr(0); i < count; i++ {
		p := *(*uintptr)(unsafe.Pointer(strs + i*8))
		if p == 0 {
			continue
		}
		n := int32(-1)
		if lens != 0 {
			n = *(*int32)(unsafe.Pointer(lens + i*4))
		}
		if n < 0 {
			b.WriteString(cstr(p))
		} else {
			b.Write(unsafe.Slice((*byte)(unsafe.Pointer(p)), int(n)))
		}
	}
	src, changed := relaxGLSL(b.String())
	if changed {
		if n := glslRelaxed.Add(1); debugGFX && n <= 3 {
			writeStderrRaw(fmt.Sprintf("rbxrt: gfx: shader %d resubmitted as GLSL ES 3.10 with implicit conversions\n", shader))
		}
	}
	buf := append([]byte(src), 0)
	ptrs := [1]uintptr{uintptr(unsafe.Pointer(&buf[0]))}
	callHostC(glShaderSourceHost, &[9]uintptr{shader, 1, uintptr(unsafe.Pointer(&ptrs[0])), 0})
	return 0
}
