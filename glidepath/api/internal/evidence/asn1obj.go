package evidence

// asn1Obj is @sigstore/core's ASN1Obj: a DER element as one tag byte, its content, and its
// children. Like the original, an OCTET STRING whose content parses as DER is given children too
// (that is how an extension's extnValue and CMS eContent are unwrapped), and re-encoding rebuilds
// the element from its children, so removing one (the SCT extension) yields the DER the SCT was
// signed over.

import (
	"encoding/asn1"
	"errors"
	"strings"
)

type asn1Obj struct {
	tag   byte
	value []byte // the content octets
	subs  []*asn1Obj
}

const asn1MaxDepth = 100

func parseASN1(der []byte) (*asn1Obj, error) {
	o, _, err := parseASN1At(der, 0, 0)
	return o, err
}

func parseASN1At(b []byte, pos, depth int) (*asn1Obj, int, error) {
	if depth > asn1MaxDepth {
		return nil, 0, errors.New("maximum nesting depth exceeded")
	}
	if pos >= len(b) {
		return nil, 0, errors.New("request past end of buffer")
	}
	tag := b[pos]
	pos++
	n, pos, err := asn1Length(b, pos)
	if err != nil {
		return nil, 0, err
	}
	if pos+n > len(b) {
		return nil, 0, errors.New("request past end of buffer")
	}
	o := &asn1Obj{tag: tag, value: b[pos : pos+n]}
	switch {
	case tag&0x20 != 0:
		if o.subs, err = collectASN1(b, pos, n, depth); err != nil {
			return nil, 0, err
		}
	case tag == 0x04:
		if subs, err := collectASN1(b, pos, n, depth); err == nil {
			o.subs = subs
		}
	}
	return o, pos + n, nil
}

func collectASN1(b []byte, pos, n, depth int) ([]*asn1Obj, error) {
	end := pos + n
	var subs []*asn1Obj
	for pos < end {
		o, next, err := parseASN1At(b[:end], pos, depth+1)
		if err != nil {
			return nil, err
		}
		subs = append(subs, o)
		pos = next
	}
	return subs, nil
}

// asn1Length is @sigstore/core's decodeLength: short form, or minimal long form of at most 6 octets.
func asn1Length(b []byte, pos int) (int, int, error) {
	if pos >= len(b) {
		return 0, 0, errors.New("request past end of buffer")
	}
	first := b[pos]
	pos++
	if first&0x80 == 0 {
		return int(first), pos, nil
	}
	count := int(first & 0x7f)
	if count > 6 {
		return 0, 0, errors.New("length exceeds 6 byte limit")
	}
	n := 0
	for i := 0; i < count; i++ {
		if pos >= len(b) {
			return 0, 0, errors.New("request past end of buffer")
		}
		if i == 0 && b[pos] == 0 {
			return 0, 0, errors.New("non-minimal length encoding")
		}
		n = n<<8 | int(b[pos])
		pos++
	}
	if n == 0 {
		return 0, 0, errors.New("indefinite length encoding not supported")
	}
	if n < 0x80 {
		return 0, 0, errors.New("non-minimal length encoding")
	}
	return n, pos, nil
}

// toDER re-encodes the element: from its children when it has any, else its content as is.
func (o *asn1Obj) toDER() []byte {
	content := o.value
	if len(o.subs) > 0 {
		content = nil
		for _, s := range o.subs {
			content = append(content, s.toDER()...)
		}
	}
	return append(append([]byte{o.tag}, encodeLength(len(content))...), content...)
}

func encodeLength(n int) []byte {
	if n < 0x80 {
		return []byte{byte(n)}
	}
	var out []byte
	for ; n > 0; n >>= 8 {
		out = append([]byte{byte(n)}, out...)
	}
	return append([]byte{0x80 | byte(len(out))}, out...)
}

func (o *asn1Obj) sub(i int) *asn1Obj {
	if o == nil || i < 0 || i >= len(o.subs) {
		return nil
	}
	return o.subs[i]
}

func (o *asn1Obj) last() *asn1Obj {
	if o == nil {
		return nil
	}
	return o.sub(len(o.subs) - 1)
}

// contextSpecific is ASN1Tag.isContextSpecific(n): class bits 10, tag number n.
func (o *asn1Obj) contextSpecific(n byte) bool {
	return o != nil && o.tag&0xc0 == 0x80 && o.tag&0x1f == n
}

// findContext is subs.find(sub => sub.tag.isContextSpecific(n)).
func (o *asn1Obj) findContext(n byte) *asn1Obj {
	if o == nil {
		return nil
	}
	for _, s := range o.subs {
		if s.contextSpecific(n) {
			return s
		}
	}
	return nil
}

// oid is toOID(): the dotted form, "" when the element is not an OID.
func (o *asn1Obj) oid() string {
	if o == nil || o.tag != 0x06 {
		return ""
	}
	var id asn1.ObjectIdentifier
	if _, err := asn1.Unmarshal(append([]byte{0x06}, append(encodeLength(len(o.value)), o.value...)...), &id); err != nil {
		return ""
	}
	return id.String()
}

// ascii is Buffer.toString('ascii'): each byte's low seven bits.
func ascii(b []byte) string {
	var s strings.Builder
	for _, c := range b {
		s.WriteByte(c & 0x7f)
	}
	return s.String()
}
