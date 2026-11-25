package hostabi

import (
	"encoding/binary"
	"net"
	"testing"
	"unsafe"
)

// testResultCell receives getaddrinfo's result pointer. It is package-level so
// the compiler cannot assume a write made through a uintptr left it zero.
var testResultCell uintptr

func cbytes(s string) uintptr {
	b := append([]byte(s), 0)
	return uintptr(unsafe.Pointer(&b[0]))
}

func TestGetaddrinfoNumeric(t *testing.T) {
	hints := addrinfo{ai_family: afInet, ai_socktype: sockStream}
	resp := &testResultCell
	rc := goGetaddrinfo(cbytes("10.1.2.3"), cbytes("443"), uintptr(unsafe.Pointer(&hints)), uintptr(unsafe.Pointer(resp)))
	res := *resp
	if rc != 0 || res == 0 {
		t.Fatalf("rc=%d res=%#x", rc, res)
	}
	defer goFreeaddrinfo(res)
	ai := (*addrinfo)(unsafe.Pointer(res))
	if ai.ai_family != afInet || ai.ai_socktype != sockStream || ai.ai_protocol != ipprotoTCP || ai.ai_addrlen != 16 {
		t.Fatalf("bad entry %+v", *ai)
	}
	sa := unsafe.Slice(ai.ai_addr, 16)
	if f := binary.LittleEndian.Uint16(sa[0:2]); f != afInet {
		t.Fatalf("sin_family=%d", f)
	}
	if p := binary.BigEndian.Uint16(sa[2:4]); p != 443 {
		t.Fatalf("sin_port=%d", p)
	}
	if ip := net.IP(sa[4:8]).String(); ip != "10.1.2.3" {
		t.Fatalf("sin_addr=%s", ip)
	}
	if ai.ai_next != nil {
		t.Fatalf("expected a single entry")
	}
	if s := sockaddrString(uintptr(unsafe.Pointer(ai.ai_addr)), 16); s != "10.1.2.3:443" {
		t.Fatalf("sockaddrString=%q", s)
	}
}

func TestGetaddrinfoErrorsAreBionic(t *testing.T) {
	hints := addrinfo{ai_flags: aiNumerichost}
	resp := &testResultCell
	if rc := goGetaddrinfo(cbytes("not-an-ip.invalid"), 0, uintptr(unsafe.Pointer(&hints)), uintptr(unsafe.Pointer(resp))); rc != eaiNoname {
		t.Fatalf("AI_NUMERICHOST on a name: rc=%d, want EAI_NONAME (%d)", rc, eaiNoname)
	}
	bad := addrinfo{ai_family: 99}
	if rc := goGetaddrinfo(cbytes("1.2.3.4"), 0, uintptr(unsafe.Pointer(&bad)), uintptr(unsafe.Pointer(resp))); rc != eaiFamily {
		t.Fatalf("bad family: rc=%d, want EAI_FAMILY (%d)", rc, eaiFamily)
	}
}

func TestGetaddrinfoLocalhost(t *testing.T) {
	resp := &testResultCell
	hints := addrinfo{ai_socktype: sockStream}
	rc := goGetaddrinfo(cbytes("localhost"), cbytes("80"), uintptr(unsafe.Pointer(&hints)), uintptr(unsafe.Pointer(resp)))
	if rc != 0 {
		t.Fatalf("localhost: rc=%d", rc)
	}
	if *resp == 0 {
		t.Fatal("localhost: no result")
	}
	goFreeaddrinfo(*resp)
}

func TestInetNtop(t *testing.T) {
	src := &[4]byte{192, 168, 1, 7}
	dst := new([16]byte)
	if goInetNtop(afInet, uintptr(unsafe.Pointer(&src[0])), uintptr(unsafe.Pointer(&dst[0])), 16) == 0 {
		t.Fatal("inet_ntop failed")
	}
	if got := cstr(uintptr(unsafe.Pointer(&dst[0]))); got != "192.168.1.7" {
		t.Fatalf("got %q", got)
	}
}
