package evidence

// Signed certificate timestamps: @sigstore/verify's verifySCTs and @sigstore/core's
// SignedCertificateTimestamp (RFC 6962 section 3.2). Each SCT in the leaf's SCT extension is
// checked against the CT logs valid at its time; the signed data is the precertificate: the
// issuer's key hash and the leaf's TBSCertificate re-encoded without the SCT extension.

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"errors"
)

const oidSCTExtension = "1.3.6.1.4.1.11129.2.4.2"

var errPastEnd = errors.New("request past end of buffer")

// verifySCTs returns the log ID of every SCT on path[0], issued by path[1]; none when it has no
// SCT extension.
func (tm *trustMaterial) verifySCTs(path []*x509.Certificate) ([][]byte, error) {
	cert, err := parseASN1(path[0].Raw)
	if err != nil {
		return nil, err
	}
	tbs := cert.sub(0)
	exts := tbs.findContext(3).sub(0)
	var sctExt *asn1Obj
	if exts != nil {
		for i, ext := range exts.subs {
			if ext.sub(0).oid() == oidSCTExtension {
				sctExt = ext
				exts.subs = append(exts.subs[:i:i], exts.subs[i+1:]...)
				break
			}
		}
	}
	if sctExt == nil {
		return nil, nil
	}
	list := sctExt.last().sub(0)
	if list == nil {
		return nil, errors.New("Cannot read properties of undefined (reading 'value')")
	}
	scts, err := parseSCTList(list.value)
	if err != nil || len(scts) == 0 {
		return nil, err
	}
	if len(path) < 2 {
		return nil, errors.New("Cannot read properties of undefined (reading 'publicKey')")
	}
	issuerID := sha256.Sum256(path[1].RawSubjectPublicKeyInfo)
	der := tbs.toDER()
	preCert := append(issuerID[:], byte(len(der)>>16), byte(len(der)>>8), byte(len(der)))
	preCert = append(preCert, der...)

	var ids [][]byte
	for _, s := range scts {
		at := float64(int64(binary.BigEndian.Uint64(s.timestamp)))
		verified := false
		for _, log := range tm.ctlogs {
			if string(log.logID) == string(s.logID) && log.validAt(at) && s.verify(preCert, log) {
				verified = true
				break
			}
		}
		if !verified {
			return nil, verifyError("SCT verification failed")
		}
		ids = append(ids, s.logID)
	}
	return ids, nil
}

type sct struct {
	version          byte
	logID, timestamp []byte
	extensions       []byte
	hashAlg, sigAlg  byte
	signature        []byte
}

type byteStream struct {
	b   []byte
	pos int
}

func (s *byteStream) block(n int) ([]byte, error) {
	if s.pos+n > len(s.b) {
		return nil, errPastEnd
	}
	out := s.b[s.pos : s.pos+n]
	s.pos += n
	return out, nil
}

func (s *byteStream) uint(n int) (int, error) {
	b, err := s.block(n)
	if err != nil {
		return 0, err
	}
	v := 0
	for _, c := range b {
		v = v<<8 | int(c)
	}
	return v, nil
}

// parseSCTList is X509SCTExtension.signedCertificateTimestamps.
func parseSCTList(buf []byte) ([]sct, error) {
	s := &byteStream{b: buf}
	n, err := s.uint(2)
	if err != nil {
		return nil, err
	}
	end := n + 2
	var out []sct
	for s.pos < end {
		l, err := s.uint(2)
		if err != nil {
			return nil, err
		}
		b, err := s.block(l)
		if err != nil {
			return nil, err
		}
		one, err := parseSCT(b)
		if err != nil {
			return nil, err
		}
		out = append(out, one)
	}
	if s.pos != end {
		return nil, errors.New("SCT list length does not match actual length")
	}
	return out, nil
}

// parseSCT is SignedCertificateTimestamp.parse: TLS encoding, the whole buffer.
func parseSCT(buf []byte) (sct, error) {
	s := &byteStream{b: buf}
	var out sct
	var err error
	read := func(f func() error) {
		if err == nil {
			err = f()
		}
	}
	read(func() error { v, e := s.uint(1); out.version = byte(v); return e })
	read(func() (e error) { out.logID, e = s.block(32); return })
	read(func() (e error) { out.timestamp, e = s.block(8); return })
	read(func() error {
		n, e := s.uint(2)
		if e == nil {
			out.extensions, e = s.block(n)
		}
		return e
	})
	read(func() error { v, e := s.uint(1); out.hashAlg = byte(v); return e })
	read(func() error { v, e := s.uint(1); out.sigAlg = byte(v); return e })
	read(func() error {
		n, e := s.uint(2)
		if e == nil {
			out.signature, e = s.block(n)
		}
		return e
	})
	if err != nil {
		return sct{}, err
	}
	if s.pos != len(buf) {
		return sct{}, errors.New("SCT buffer length mismatch")
	}
	return out, nil
}

// verify checks the SCT's signature over RFC 6962's digitally-signed struct for a precert entry.
func (s sct) verify(preCert []byte, log tlogAuthority) bool {
	data := []byte{s.version, 0x00} // signature_type certificate_timestamp
	data = append(data, s.timestamp...)
	data = append(data, 0x00, 0x01) // entry_type precert_entry
	data = append(data, preCert...)
	data = append(data, byte(len(s.extensions)>>8), byte(len(s.extensions)))
	data = append(data, s.extensions...)
	alg := map[byte]string{4: "sha256", 5: "sha384", 6: "sha512"}[s.hashAlg]
	if alg == "" {
		alg = "unsupported"
	}
	return verifySig(data, log.key, s.signature, alg)
}
