package evidence

// RFC 3161 timestamps: @sigstore/core's RFC3161Timestamp/TSTInfo and @sigstore/verify's
// verifyRFC3161Timestamp. Only the promotion attestation carries one (public Sigstore's
// timestamp.sigstore.dev countersigns the signature); the platform trusts no timestamp authority,
// so there every timestamp fails, as it does in Tower.

import (
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"time"
)

var (
	sha2HashAlgos   = map[string]string{"2.16.840.1.101.3.4.2.1": "sha256", "2.16.840.1.101.3.4.2.2": "sha384", "2.16.840.1.101.3.4.2.3": "sha512"}
	ecdsaSignAlgos  = map[string]string{"1.2.840.10045.4.3.1": "sha224", "1.2.840.10045.4.3.2": "sha256", "1.2.840.10045.4.3.3": "sha384", "1.2.840.10045.4.3.4": "sha512"}
	errTSANotParsed = errors.New("Cannot read properties of undefined (reading 'subs')")
)

const (
	oidSignedData    = "1.2.840.113549.1.7.2"
	oidTSTInfo       = "1.2.840.113549.1.9.16.1.4"
	oidMessageDigest = "1.2.840.113549.1.9.4"
)

type rfc3161Timestamp struct{ root *asn1Obj }

func parseRFC3161(der []byte) (*rfc3161Timestamp, error) {
	root, err := parseASN1(der)
	if err != nil {
		return nil, err
	}
	return &rfc3161Timestamp{root}, nil
}

func (t *rfc3161Timestamp) token() *asn1Obj      { return t.root.sub(1) }
func (t *rfc3161Timestamp) signedData() *asn1Obj { return t.token().findContext(0).sub(0) }
func (t *rfc3161Timestamp) encap() *asn1Obj      { return t.signedData().sub(2) }
func (t *rfc3161Timestamp) signerInfo() *asn1Obj { return t.signedData().last().sub(0) }
func (t *rfc3161Timestamp) tstInfo() *asn1Obj    { return t.encap().sub(1).sub(0).sub(0) }

// signingTime is tstInfo.genTime (its fifth element).
func (t *rfc3161Timestamp) signingTime() (time.Time, error) {
	g := t.tstInfo().sub(4)
	if g == nil {
		return time.Time{}, errTSANotParsed
	}
	var at time.Time
	if _, err := asn1.UnmarshalWithParams(append([]byte{g.tag}, append(encodeLength(len(g.value)), g.value...)...), &at, ""); err != nil {
		return time.Time{}, errors.New("not a date")
	}
	return at, nil
}

func (t *rfc3161Timestamp) signerIssuer() []byte { return t.signerInfo().sub(1).sub(0).valueOrNil() }
func (t *rfc3161Timestamp) signerSerial() []byte { return t.signerInfo().sub(1).sub(1).valueOrNil() }

func (o *asn1Obj) valueOrNil() []byte {
	if o == nil {
		return nil
	}
	return o.value
}

// verify is RFC3161Timestamp.verify: content types, the message imprint over data, the signed
// message digest over the TSTInfo, and the signature over the signed attributes.
func (t *rfc3161Timestamp) verify(data []byte, signer *x509.Certificate) error {
	if t.token() == nil {
		return errors.New("timeStampToken is missing")
	}
	if ct := t.token().sub(0).oid(); ct != oidSignedData {
		return errors.New("incorrect content type: " + ct)
	}
	if ect := t.encap().sub(0).oid(); ect != oidTSTInfo {
		return errors.New("incorrect encapsulated content type: " + ect)
	}
	info := t.tstInfo()
	if info == nil {
		return errTSANotParsed
	}
	imprint := info.sub(2)
	if !bytes.Equal(digest(sha2HashAlgos[imprint.sub(0).sub(0).oid()], data), imprint.sub(1).valueOrNil()) {
		return errors.New("message imprint does not match artifact")
	}
	si := t.signerInfo()
	attrs := si.findContext(0)
	if attrs == nil {
		return errTSANotParsed
	}
	var expected []byte
	for _, a := range attrs.subs {
		if a.sub(0).oid() == oidMessageDigest {
			expected = a.sub(1).sub(0).valueOrNil()
			break
		}
	}
	if !bytes.Equal(digest(sha2HashAlgos[si.sub(2).sub(0).oid()], info.toDER()), expected) {
		return errors.New("signed data does not match tstInfo")
	}
	signed := attrs.toDER()
	signed[0] = 0x31 // the context-specific [0] is signed as a SET
	alg, known := ecdsaSignAlgos[si.sub(4).sub(0).oid()]
	if !known {
		alg = "unknown"
	}
	if !verifySig(signed, signer.PublicKey, si.sub(5).valueOrNil(), alg) {
		return errors.New("signature verification failed")
	}
	return nil
}

func digest(alg string, data []byte) []byte {
	switch alg {
	case "sha256":
		h := sha256.Sum256(data)
		return h[:]
	case "sha384":
		h := sha512.Sum384(data)
		return h[:]
	case "sha512":
		h := sha512.Sum512(data)
		return h[:]
	}
	return nil
}

// verifyRFC3161 is verifyRFC3161Timestamp: the authorities valid at the signing time whose leaf
// is the timestamp's signer (serial and issuer), and at least one whose chain and signature check.
// It returns the signing time.
func (tm *trustMaterial) verifyRFC3161(t *rfc3161Timestamp, data []byte) (time.Time, error) {
	at, err := t.signingTime()
	if err != nil {
		return time.Time{}, err
	}
	issuer, serial := t.signerIssuer(), t.signerSerial()
	for _, ca := range tm.tsas {
		if !ca.validAt(ms(at)) || len(ca.chain) == 0 {
			continue
		}
		leaf := ca.chain[0]
		if !bytes.Equal(certSerial(leaf), serial) || !bytes.Equal(nameContent(leaf.RawIssuer), issuer) {
			continue
		}
		if _, err := chainVerify(ca.chain[1:], leaf, ms(at)); err != nil {
			continue
		}
		if t.verify(data, leaf) == nil {
			return at, nil
		}
	}
	return time.Time{}, verifyError("timestamp could not be verified")
}

// certSerial is X509Certificate.serialNumber: the INTEGER's content octets.
func certSerial(c *x509.Certificate) []byte {
	tbs, err := parseASN1(c.RawTBSCertificate)
	if err != nil {
		return nil
	}
	i := 0
	if tbs.sub(0).contextSpecific(0) { // version
		i = 1
	}
	return tbs.sub(i).valueOrNil()
}

// nameContent is X509Certificate.issuer: the Name's content octets, without its SEQUENCE header.
func nameContent(raw []byte) []byte {
	o, err := parseASN1(raw)
	if err != nil {
		return nil
	}
	return o.value
}
