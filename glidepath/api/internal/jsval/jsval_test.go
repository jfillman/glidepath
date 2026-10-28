package jsval

import (
	"encoding/hex"
	"encoding/json"
	"math"
	"testing"
)

func TestOrderAndUndefined(t *testing.T) {
	v, err := Parse([]byte(`{"b":1,"2":"two","a":null,"10":[1,null],"b":3,"1":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	o := v.(*Obj)
	o.Set("gone", Undefined).Set("a", "kept in place")
	got, _ := json.Marshal(o)
	// Index keys first ascending, then insertion order; a duplicate keeps its first place and last value.
	if string(got) != `{"1":{},"2":"two","10":[1,null],"b":3,"a":"kept in place"}` {
		t.Fatal(string(got))
	}
	got, _ = json.Marshal(Arr{1.0, Undefined, Num(math.NaN())})
	if string(got) != `[1,null,null]` {
		t.Fatal(string(got))
	}
	if _, err := Parse([]byte(`{} x`)); err == nil {
		t.Fatal("trailing data")
	}
}

func TestBufferDecoding(t *testing.T) {
	// Node 26's Buffer.from(s, 'base64') and (s, 'hex').
	for in, want := range map[string]string{"aGVsbG8=": "68656c6c6f", "aGVs bG8": "68656c6c6f", "aGV$sbG8": "68656c6c6f",
		"aGVsbG8=aGk=": "68656c6c6f", "aGVsb": "68656c", "a": "", "aGVs-_8": "68656cfbff"} {
		if got := hex.EncodeToString(Base64(in)); got != want {
			t.Errorf("base64 %q: %s, want %s", in, got, want)
		}
	}
	for in, want := range map[string]string{"abcd": "abcd", "abc": "ab", "abzz12": "ab", "AbCd": "abcd"} {
		if got := hex.EncodeToString(Hex(in)); got != want {
			t.Errorf("hex %q: %s, want %s", in, got, want)
		}
	}
}

func TestNumberAndString(t *testing.T) {
	for in, want := range map[string]float64{"": 0, " 12 ": 12, "0x10": 16, "1e3": 1000, "2.5": 2.5} {
		if got := Number(in); got != want {
			t.Errorf("Number(%q) = %v", in, got)
		}
	}
	for _, in := range []any{"abc", "inf", "1_000", Undefined} {
		if !math.IsNaN(Number(in)) {
			t.Errorf("Number(%v) is NaN", in)
		}
	}
	if Number(nil) != 0 || String(1e21) != "1e+21" || String(2879.0) != "2879" || String(nil) != "null" || String([]any{1.0, nil, "x"}) != "1,,x" {
		t.Fatal("String/Number")
	}
	if Truthy(0.0) || Truthy("") || Truthy(nil) || Truthy(Undefined) || !Truthy(NewObj()) || !Truthy("0") {
		t.Fatal("Truthy")
	}
}
