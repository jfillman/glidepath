// Package jsval is JSON as Tower's TypeScript sees it, for the ports that must answer field for
// field like Tower does (internal/evidence, internal/testkube).
//
// Three things differ from encoding/json's map[string]any and matter for that comparison:
//   - undefined is not null. A key read from nowhere is Undefined, and an object holding Undefined
//     drops the key when it is written, as JSON.stringify does; a JSON null is kept.
//   - Objects keep their key order: integer-like keys first, ascending, then the rest in insertion
//     order (ECMAScript's own property order), with a duplicate key keeping its first position.
//   - Truthiness, ?? and String() follow JavaScript.
//
// Numbers are float64, as in JavaScript.
package jsval

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

type undefined struct{}

// Undefined is JavaScript's undefined: an absent key or value.
var Undefined any = undefined{}

// Obj is a JSON object with JavaScript's key order.
type Obj struct {
	keys []string
	vals map[string]any
}

func NewObj() *Obj { return &Obj{vals: map[string]any{}} }

// Set adds or replaces k. A replaced key keeps its position, as an object spread does.
func (o *Obj) Set(k string, v any) *Obj {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
	return o
}

// Get returns k's value, or Undefined.
func (o *Obj) Get(k string) any {
	if o == nil {
		return Undefined
	}
	if v, ok := o.vals[k]; ok {
		return v
	}
	return Undefined
}

// Keys returns the keys in JavaScript's property order.
func (o *Obj) Keys() []string {
	if o == nil {
		return nil
	}
	var ints []string
	var rest []string
	for _, k := range o.keys {
		if isIndex(k) {
			ints = append(ints, k)
		} else {
			rest = append(rest, k)
		}
	}
	sort.Slice(ints, func(i, j int) bool {
		a, _ := strconv.ParseUint(ints[i], 10, 32)
		b, _ := strconv.ParseUint(ints[j], 10, 32)
		return a < b
	})
	return append(ints, rest...)
}

// Clone copies o one level deep: {...o}.
func (o *Obj) Clone() *Obj {
	c := NewObj()
	for _, k := range o.Keys() {
		c.Set(k, o.vals[k])
	}
	return c
}

// isIndex: an array-index property name, which ECMAScript orders before every other key.
func isIndex(k string) bool {
	if k == "" || len(k) > 10 || (len(k) > 1 && k[0] == '0') {
		return false
	}
	n, err := strconv.ParseUint(k, 10, 64)
	return err == nil && n < math.MaxUint32
}

func (o *Obj) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	first := true
	for _, k := range o.Keys() {
		v := o.vals[k]
		if v == Undefined {
			continue
		}
		if !first {
			b.WriteByte(',')
		}
		first = false
		kb, _ := marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		vb, err := marshal(v)
		if err != nil {
			return nil, err
		}
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// Arr is a JSON array whose Undefined elements are written as null, as JSON.stringify does.
type Arr []any

func (a Arr) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('[')
	for i, v := range a {
		if i > 0 {
			b.WriteByte(',')
		}
		if v == Undefined {
			v = nil
		}
		vb, err := marshal(v)
		if err != nil {
			return nil, err
		}
		b.Write(vb)
	}
	b.WriteByte(']')
	return b.Bytes(), nil
}

// Num is a JavaScript number: NaN and the infinities are written as null.
type Num float64

func (n Num) MarshalJSON() ([]byte, error) {
	f := float64(n)
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return []byte("null"), nil
	}
	return json.Marshal(f)
}

func marshal(v any) ([]byte, error) {
	switch x := v.(type) {
	case float64:
		return Num(x).MarshalJSON()
	case []any:
		return Arr(x).MarshalJSON()
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// Parse is JSON.parse: objects become *Obj, arrays []any, numbers float64.
func Parse(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	v, err := parseValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("Unexpected non-whitespace character after JSON")
	}
	return v, nil
}

func parseValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := NewObj()
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, _ := kt.(string)
				v, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				o.Set(k, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return o, nil
		case '[':
			a := []any{}
			for dec.More() {
				v, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				a = append(a, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return a, nil
		}
		return nil, errors.New("unexpected delimiter")
	default:
		return t, nil // string, float64, bool, nil
	}
}

// Get reads v[k] the way v?.k does: Undefined unless v is an object holding k.
func Get(v any, k string) any {
	if o, ok := v.(*Obj); ok {
		return o.Get(k)
	}
	return Undefined
}

// Path is v?.a?.b?.c.
func Path(v any, keys ...string) any {
	for _, k := range keys {
		v = Get(v, k)
	}
	return v
}

// Nullish: null or undefined.
func Nullish(v any) bool { return v == nil || v == Undefined }

// Or is a ?? b.
func Or(a, b any) any {
	if Nullish(a) {
		return b
	}
	return a
}

// Truthy is JavaScript's truthiness.
func Truthy(v any) bool {
	switch x := v.(type) {
	case nil, undefined:
		return false
	case bool:
		return x
	case float64:
		return x != 0 && !math.IsNaN(x)
	case string:
		return x != ""
	}
	return true
}

// String is JavaScript's String(v).
func String(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case undefined:
		return "undefined"
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return NumberString(x)
	case string:
		return x
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			if !Nullish(e) {
				parts[i] = String(e)
			}
		}
		return strings.Join(parts, ",")
	}
	return "[object Object]"
}

// NumberString is a number's String(): integers without a fraction, exponents past 1e21.
func NumberString(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	b, _ := json.Marshal(f)
	return string(b)
}

// Number is JavaScript's Number(v), for the values the ports convert: strings, numbers, absent.
func Number(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case bool:
		if x {
			return 1
		}
		return 0
	case nil:
		return 0
	case string:
		return numberFromString(x)
	}
	return math.NaN()
}

func numberFromString(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	switch s {
	case "Infinity", "+Infinity":
		return math.Inf(1)
	case "-Infinity":
		return math.Inf(-1)
	}
	if len(s) > 2 && s[0] == '0' {
		base := 0
		switch s[1] {
		case 'x', 'X':
			base = 16
		case 'o', 'O':
			base = 8
		case 'b', 'B':
			base = 2
		}
		if base != 0 {
			n, err := strconv.ParseUint(s[2:], base, 64)
			if err != nil || strings.Contains(s, "_") {
				return math.NaN()
			}
			return float64(n)
		}
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-') {
			return math.NaN()
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		var ne *strconv.NumError
		if errors.As(err, &ne) && errors.Is(ne.Err, strconv.ErrRange) {
			return f
		}
		return math.NaN()
	}
	return f
}

// Base64 is Buffer.from(s, 'base64'): both alphabets, anything else skipped, stopping at the first
// '=', and a trailing lone character dropped. Node never fails to decode, so neither does this.
func Base64(s string) []byte {
	clean := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '=':
			i = len(s)
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '+', c == '/':
			clean = append(clean, c)
		case c == '-':
			clean = append(clean, '+')
		case c == '_':
			clean = append(clean, '/')
		}
	}
	if len(clean)%4 == 1 {
		clean = clean[:len(clean)-1]
	}
	out, _ := base64.RawStdEncoding.DecodeString(string(clean))
	return out
}

// Hex is Buffer.from(s, 'hex'): it stops at the first pair that isn't hex.
func Hex(s string) []byte {
	out := make([]byte, 0, len(s)/2)
	for i := 0; i+1 < len(s); i += 2 {
		b, err := hex.DecodeString(s[i : i+2])
		if err != nil {
			break
		}
		out = append(out, b[0])
	}
	return out
}

// Index is v?.[i] for an array: Undefined past its end or for anything else.
func Index(v any, i int) any {
	if a, ok := v.([]any); ok && i >= 0 && i < len(a) {
		return a[i]
	}
	return Undefined
}
