package evidence

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"fmt"
	"strings"
	"time"

	"github.com/glidepath/api/internal/jsval"
)

// Certificate is Tower's CertificateSummary, in the text Node's X509Certificate gives: subject and
// issuer one RDN per line (OpenSSL short names, RFC 2253 escaping), dates as "Oct 21 06:12:16 2026
// GMT", identity the first URI SAN (where Fulcio puts the signer's identity). An empty name (a
// Fulcio leaf has no subject) is absent, as Node leaves it undefined.
type Certificate struct {
	Subject   *string `json:"subject,omitempty"`
	Issuer    *string `json:"issuer,omitempty"`
	Identity  *string `json:"identity,omitempty"`
	ValidFrom string  `json:"validFrom"`
	ValidTo   string  `json:"validTo"`
}

// summarizeCertificate: undefined (nil) for anything that doesn't parse, as Tower's does.
func summarizeCertificate(certPem string) *Certificate {
	block, _ := pem.Decode([]byte(certPem))
	if block == nil {
		return nil
	}
	cert, err := parseCert(block.Bytes)
	if err != nil {
		return nil
	}
	return &Certificate{
		Subject:   nodeName(cert.RawSubject),
		Issuer:    nodeName(cert.RawIssuer),
		Identity:  firstURISAN(cert),
		ValidFrom: nodeTime(cert.NotBefore),
		ValidTo:   nodeTime(cert.NotAfter),
	}
}

// parseCert parses the first DER element and ignores anything after it, as @sigstore/core's
// X509Certificate.parse and Node's X509Certificate do.
func parseCert(der []byte) (*x509.Certificate, error) {
	var first asn1.RawValue
	if _, err := asn1.Unmarshal(der, &first); err != nil {
		return nil, err
	}
	return x509.ParseCertificate(first.FullBytes)
}

// nodeTime is OpenSSL's ASN1_TIME_print, which Node's validFrom/validTo use.
func nodeTime(t time.Time) string { return t.UTC().Format("Jan _2 15:04:05 2006 GMT") }

var shortNames = map[string]string{
	"2.5.4.3": "CN", "2.5.4.4": "SN", "2.5.4.5": "serialNumber", "2.5.4.6": "C", "2.5.4.7": "L",
	"2.5.4.8": "ST", "2.5.4.9": "street", "2.5.4.10": "O", "2.5.4.11": "OU", "2.5.4.12": "title",
	"2.5.4.17": "postalCode", "2.5.4.42": "GN", "2.5.4.43": "initials", "2.5.4.44": "generationQualifier",
	"2.5.4.46": "dnQualifier", "2.5.4.65": "pseudonym", "1.2.840.113549.1.9.1": "emailAddress",
	"0.9.2342.19200300.100.1.25": "DC", "0.9.2342.19200300.100.1.1": "UID",
}

// nodeName is X509_NAME_print_ex with Node's flags: RDNs in certificate order, one per line,
// multi-valued RDNs joined with " + ", values escaped per RFC 2253.
func nodeName(raw []byte) *string {
	var seq pkix.RDNSequence
	if _, err := asn1.Unmarshal(raw, &seq); err != nil || len(seq) == 0 {
		return nil
	}
	lines := make([]string, 0, len(seq))
	for _, rdn := range seq {
		parts := make([]string, 0, len(rdn))
		for _, atv := range rdn {
			name, ok := shortNames[atv.Type.String()]
			if !ok {
				name = atv.Type.String()
			}
			parts = append(parts, name+"="+escape2253(fmt.Sprint(atv.Value)))
		}
		lines = append(lines, strings.Join(parts, " + "))
	}
	name := strings.Join(lines, "\n")
	return &name
}

func escape2253(v string) string {
	var b strings.Builder
	for i, r := range v {
		switch {
		case strings.ContainsRune(`,+"\<>;`, r),
			i == 0 && (r == '#' || r == ' '),
			i == len(v)-1 && r == ' ':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, "\\%02X", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// firstURISAN is Tower's cert.subjectAltName.split(', ').find(e => e.startsWith('URI:')).slice(4):
// the first URI SAN in certificate order. Node quotes a SAN value holding a comma, quote,
// backslash or control character, so such a value comes back quoted, as it does in Tower.
func firstURISAN(cert *x509.Certificate) *string {
	for _, ext := range cert.Extensions {
		if !ext.Id.Equal(oidSAN) {
			continue
		}
		var names []asn1.RawValue
		if _, err := asn1.Unmarshal(ext.Value, &names); err != nil {
			return nil
		}
		for _, n := range names {
			if n.Class != asn1.ClassContextSpecific || n.Tag != 6 {
				continue
			}
			u := string(n.Bytes)
			if strings.ContainsAny(u, `,"\`) || strings.IndexFunc(u, func(r rune) bool { return r < 0x20 || r > 0x7e }) >= 0 {
				u, _ = jsonString(u)
			}
			return &u
		}
	}
	return nil
}

var oidSAN = asn1.ObjectIdentifier{2, 5, 29, 17}

func jsonString(s string) (string, error) {
	b, err := jsval.NewObj().Set("s", s).MarshalJSON()
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(strings.TrimPrefix(string(b), `{"s":`), "}"), nil
}
