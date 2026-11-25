package hostabi

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unsafe"
)

func TestRelaxGLSL(t *testing.T) {
	src := "#version 300 es\nuniform vec4 CB12[4];\nvoid main(){}\n"
	got, ok := relaxGLSL(src)
	if !ok || !strings.HasPrefix(got, glslES310+"\n") || !strings.HasSuffix(got, "void main(){}\n") {
		t.Fatalf("relaxGLSL(ES 3.00) = %q, %v", got, ok)
	}
	for _, keep := range []string{"#version 310 es\nvoid main(){}", "#version 100\n", "void main(){}", "#version 300 esx\n"} {
		if got, ok := relaxGLSL(keep); ok || got != keep {
			t.Errorf("relaxGLSL(%q) changed it to %q", keep, got)
		}
	}
}

// TestGLSLRelaxCorpus compiles a directory of real engine shaders on the host
// driver, as submitted and as relaxed, and requires that relaxing fixes the
// shaders that need it without breaking any that compiled already. Produce
// the corpus with MESA_SHADER_DUMP_PATH=<dir> during a launch, then:
//
//	RBXRT_GFX_TEST=1 RBXRT_GLSL_CORPUS=<dir> go test ./internal/hostabi -run TestGLSLRelaxCorpus -v
func TestGLSLRelaxCorpus(t *testing.T) {
	dir := os.Getenv("RBXRT_GLSL_CORPUS")
	if os.Getenv("RBXRT_GFX_TEST") == "" || dir == "" {
		t.Skip("set RBXRT_GFX_TEST=1 and RBXRT_GLSL_CORPUS=<dir of VS_*/FS_*.glsl>")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	setupProbeContext(t)

	gl := func(name string) uintptr {
		a, _ := resolveGPU(name)
		if a == 0 {
			t.Fatalf("host libGLESv2 is missing %s", name)
		}
		return a
	}
	glCreateShader, glShaderSource := gl("glCreateShader"), gl("glShaderSource")
	glCompileShader, glGetShaderiv := gl("glCompileShader"), gl("glGetShaderiv")
	glDeleteShader := gl("glDeleteShader")
	compiles := func(kind uintptr, src string) bool {
		sh := callHostC(glCreateShader, &[9]uintptr{kind})
		buf := append([]byte(src), 0)
		ptrs := [1]uintptr{uintptr(unsafe.Pointer(&buf[0]))}
		callHostC(glShaderSource, &[9]uintptr{sh, 1, uintptr(unsafe.Pointer(&ptrs[0])), 0})
		callHostC(glCompileShader, &[9]uintptr{sh})
		var status int32
		callHostC(glGetShaderiv, &[9]uintptr{sh, 0x8B81, uintptr(unsafe.Pointer(&status))}) // GL_COMPILE_STATUS
		callHostC(glDeleteShader, &[9]uintptr{sh})
		return status != 0
	}

	files, _ := filepath.Glob(filepath.Join(dir, "*.glsl"))
	if len(files) == 0 {
		t.Fatalf("no *.glsl files in %s", dir)
	}
	var before, after, fixed, broken int
	for _, f := range files {
		kind := uintptr(0x8B31) // GL_VERTEX_SHADER
		if strings.HasPrefix(filepath.Base(f), "FS_") {
			kind = 0x8B30 // GL_FRAGMENT_SHADER
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		relaxed, _ := relaxGLSL(string(b))
		okBefore, okAfter := compiles(kind, string(b)), compiles(kind, relaxed)
		if okBefore {
			before++
		}
		if okAfter {
			after++
		}
		switch {
		case !okBefore && okAfter:
			fixed++
		case okBefore && !okAfter:
			broken++
			t.Errorf("relaxing broke %s", filepath.Base(f))
		case !okBefore && !okAfter:
			t.Errorf("still fails after relaxing: %s", filepath.Base(f))
		}
	}
	t.Logf("%d shaders: %d compiled as submitted, %d after relaxing (%d fixed, %d broken)",
		len(files), before, after, fixed, broken)
}
