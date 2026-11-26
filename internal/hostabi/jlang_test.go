package hostabi

import "testing"

func TestJavaEnumsAndBoxing(t *testing.T) {
	const enum = "com/example/Color"
	values := jniMember{class: enum, name: "values", sig: "()[Lcom/example/Color;"}
	a, ok := jlangStatic(enum, values, 0)
	if !ok {
		t.Fatal("values() not answered")
	}
	if b, _ := jlangStatic(enum, values, 0); b != a {
		t.Errorf("values() returned a different array the second time")
	}
	consts := ObjectArrayElements(a)
	if len(consts) != enumArraySize || consts[0] == consts[1] {
		t.Fatalf("values() has %d constants (distinct: %v)", len(consts), consts[0] != consts[1])
	}
	for _, i := range []int{0, 3} {
		if v, ok := jlangInstance(consts[i], jniMember{name: "ordinal"}); !ok || v != uintptr(i) {
			t.Errorf("values()[%d].ordinal() = %d, %v", i, v, ok)
		}
	}
	if _, ok := jlangStatic("com/example/Color", jniMember{name: "valueOf", sig: "(Ljava/lang/String;)Lcom/example/Color;"}, 0); ok {
		t.Errorf("an enum's valueOf(String) was treated as boxing")
	}

	box, ok := jlangStatic("java/lang/Long", jniMember{name: "valueOf", sig: "(J)Ljava/lang/Long;"}, 1234)
	if !ok || jniObjectClass(box) != "java/lang/Long" {
		t.Fatalf("Long.valueOf not answered")
	}
	if v, ok := jlangInstance(box, jniMember{name: "longValue"}); !ok || v != 1234 {
		t.Errorf("longValue() = %d, %v", v, ok)
	}
}
