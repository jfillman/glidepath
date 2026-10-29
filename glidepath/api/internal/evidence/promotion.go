package evidence

// The Flight promotion attestation (glidepath ADR-0029), ported from Tower's promotionEvidence.ts.
// The promoter workflow in the Flight owner's promoter repository copies a release image from
// Ground to Flight by digest and signs this statement on the Flight digest, keyless on public
// Sigstore with its GitHub Actions identity. It is the only proof an image went through the
// promoter, so it is verified the way an upper cluster's admission does: the public-good trusted
// root (embedded, so verification stays offline), its inclusion proof, SCT and timestamp, and the
// exact workflow identity. Never the platform's Fulcio and Rekor, which did not sign it.

import (
	"crypto"
	"crypto/x509"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// PromotionPredicate is the promotion statement's predicateType.
const PromotionPredicate = "https://hangar.io/attestations/promotion/v1"

const githubActionsIssuer = "https://token.actions.githubusercontent.com"

// sigstorePublicGoodRoot is tuf-repo-cdn.sigstore.dev's target trusted_root.json, sha256
// 6494e21ea73fa7ee769f85f57d5a3e6a08725eae1e38c755fc3517c9e6bc0b66 (the hash targets.json v14
// lists), byte for byte the root Tower embeds (sigstorePublicGoodRoot.ts). Its Fulcio CA and Rekor
// keys are open-ended; refresh both copies when Sigstore rotates a key (`cosign initialize`, then
// ~/.sigstore/root/tuf-repo-cdn.sigstore.dev/targets/trusted_root.json).
//
//go:embed sigstore_public_good_trusted_root.json
var sigstorePublicGoodRoot []byte

// PromotionTrust is what a promotion attestation is verified against.
type PromotionTrust struct {
	tm *trustMaterial
	// identity matches the promoter workflow's certificate SAN and nothing else:
	// ^https://github\.com/<owner>/<repo>/\.github/workflows/promote\.yaml@refs/heads/main$
	identity string
}

var (
	publicGoodOnce sync.Once
	publicGood     *trustMaterial
	publicGoodErr  error
)

// NewPromotionTrust is Tower's promotionTrust for glidepath.promotion.promoterRepo (owner/repo):
// nil, with promotion attestations shown but never verified, when no promoter is configured.
func NewPromotionTrust(promoterRepo string) (*PromotionTrust, error) {
	if promoterRepo == "" {
		return nil, nil
	}
	publicGoodOnce.Do(func() { publicGood, publicGoodErr = parseTrustedRoot(sigstorePublicGoodRoot) })
	if publicGoodErr != nil {
		return nil, fmt.Errorf("sigstore public-good trusted root: %w", publicGoodErr)
	}
	return &PromotionTrust{
		tm: publicGood,
		// Anchored and escaped: the policy SAN is a regular expression (identityPolicy.verify), and
		// unanchored "...@refs/heads/main" would also accept a branch named main-anything.
		identity: "^" + regexp.QuoteMeta("https://github.com/"+promoterRepo+"/.github/workflows/promote.yaml@refs/heads/main") + "$",
	}, nil
}

// verifyPromotionBundle is Tower's: the bundle as it came, against public Sigstore, with one tlog
// entry, one SCT and the promoter's identity required.
func verifyPromotionBundle(raw any, trust *PromotionTrust) verdict {
	if trust == nil {
		return verdict{err: ptr("no promoter configured (glidepath.promotion.promoterRepo)")}
	}
	b, err := bundleFromJSON(raw)
	if err == nil {
		err = trust.tm.verify(b, nil, verifyOptions{tlogThreshold: 1, ctlogThreshold: 1,
			policy: &identityPolicy{subjectAlternativeName: trust.identity, issuer: githubActionsIssuer}})
	}
	if err != nil {
		return verdict{err: ptr(err.Error())}
	}
	return verdict{verified: true}
}

// identityPolicy is the VerificationPolicy Tower passes: a SAN and the Fulcio issuer extension.
type identityPolicy struct {
	subjectAlternativeName string
	issuer                 string
}

const (
	oidSANExtension  = "2.5.29.17"
	oidFulcioIssuer  = "1.3.6.1.4.1.57264.1.1"
	oidFulcioIssuer2 = "1.3.6.1.4.1.57264.1.8"
)

// verify is Verifier.verifyPolicy. The SAN check is @sigstore/verify's
// `signerIdentity.match(policyIdentity)`: the policy SAN is a regular expression, unanchored, as
// String.prototype.match makes it.
func (p *identityPolicy) verify(leaf *x509.Certificate) error {
	san, issuer := signerIdentity(leaf)
	re, err := regexp.Compile(p.subjectAlternativeName)
	if err != nil {
		return fmt.Errorf("Invalid regular expression: /%s/: %w", p.subjectAlternativeName, err)
	}
	if san == nil || !re.MatchString(*san) {
		return verifyError(fmt.Sprintf("certificate identity error - expected %s, got %s", p.subjectAlternativeName, orUndefined(san)))
	}
	if issuer == nil || *issuer != p.issuer {
		return verifyError(fmt.Sprintf("invalid certificate extension - expected issuer=%s, got issuer=%s", p.issuer, orUndefined(issuer)))
	}
	return nil
}

func orUndefined(s *string) string {
	if s == nil {
		return "undefined"
	}
	return *s
}

// signerIdentity is getSigner's identity: the SAN extension's first URI (else its first email),
// and the Fulcio issuer, from the v2 extension (a DER UTF8String) when there is one, else v1's
// raw bytes.
func signerIdentity(leaf *x509.Certificate) (san, issuer *string) {
	cert, err := parseASN1(leaf.Raw)
	if err != nil {
		return nil, nil
	}
	ext := func(oid string) *asn1Obj {
		for _, e := range cert.sub(0).findContext(3).sub(0).subsOrNil() {
			if e.sub(0).oid() == oid {
				return e
			}
		}
		return nil
	}
	if e := ext(oidSANExtension); e != nil {
		names := e.last().sub(0).subsOrNil()
		for _, tag := range []byte{0x06, 0x01} {
			for _, gn := range names {
				if gn.contextSpecific(tag) {
					s := ascii(gn.value)
					san = &s
					break
				}
			}
			if san != nil {
				break
			}
		}
	}
	if e := ext(oidFulcioIssuer2); e != nil {
		if v := e.last().sub(0); v != nil {
			s := ascii(v.value)
			issuer = &s
		}
	} else if e := ext(oidFulcioIssuer); e != nil {
		s := ascii(e.last().value)
		issuer = &s
	}
	return san, issuer
}

func (o *asn1Obj) subsOrNil() []*asn1Obj {
	if o == nil {
		return nil
	}
	return o.subs
}

// parseTrustedRoot is toTrustMaterial(TrustedRoot.fromJSON(root)): every CA, log, CT log and
// timestamp authority with its validity window (open ends are the beginning and end of time).
func parseTrustedRoot(data []byte) (*trustMaterial, error) {
	type validFor struct{ Start, End string }
	type ca struct {
		CertChain struct {
			Certificates []struct{ RawBytes string }
		}
		ValidFor *validFor
	}
	type tlog struct {
		BaseURL   string `json:"baseUrl"`
		PublicKey struct {
			RawBytes   string
			KeyDetails string
			ValidFor   *validFor
		}
		LogID           struct{ KeyID string }  `json:"logId"`
		CheckpointKeyID *struct{ KeyID string } `json:"checkpointKeyId"`
	}
	var root struct {
		Tlogs                  []tlog
		CertificateAuthorities []ca
		Ctlogs                 []tlog
		TimestampAuthorities   []ca
	}
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	window := func(v *validFor) (float64, float64, error) {
		start, end := 0.0, endOfTime
		if v == nil {
			return start, end, nil
		}
		for _, f := range []struct {
			s   string
			out *float64
		}{{v.Start, &start}, {v.End, &end}} {
			if f.s == "" {
				continue
			}
			t, err := time.Parse(time.RFC3339Nano, f.s)
			if err != nil {
				return 0, 0, err
			}
			*f.out = ms(t)
		}
		return start, end, nil
	}
	cas := func(in []ca) ([]certAuthority, error) {
		var out []certAuthority
		for _, c := range in {
			a := certAuthority{}
			for _, raw := range c.CertChain.Certificates {
				der, err := base64.StdEncoding.DecodeString(raw.RawBytes)
				if err != nil {
					return nil, err
				}
				cert, err := parseCert(der)
				if err != nil {
					return nil, err
				}
				a.chain = append(a.chain, cert)
			}
			var err error
			if a.start, a.end, err = window(c.ValidFor); err != nil {
				return nil, err
			}
			out = append(out, a)
		}
		return out, nil
	}
	logs := func(in []tlog) ([]tlogAuthority, error) {
		var out []tlogAuthority
		for _, l := range in {
			der, err := base64.StdEncoding.DecodeString(l.PublicKey.RawBytes)
			if err != nil {
				return nil, err
			}
			var key crypto.PublicKey
			if strings.HasPrefix(l.PublicKey.KeyDetails, "PKCS1_RSA") {
				key, err = x509.ParsePKCS1PublicKey(der)
			} else {
				key, err = x509.ParsePKIXPublicKey(der)
			}
			if err != nil {
				return nil, fmt.Errorf("%s: %w", l.BaseURL, err)
			}
			id := l.LogID.KeyID
			if l.CheckpointKeyID != nil {
				id = l.CheckpointKeyID.KeyID
			}
			logID, err := base64.StdEncoding.DecodeString(id)
			if err != nil {
				return nil, err
			}
			a := tlogAuthority{logID: logID, baseURL: l.BaseURL, key: key}
			if a.start, a.end, err = window(l.PublicKey.ValidFor); err != nil {
				return nil, err
			}
			out = append(out, a)
		}
		return out, nil
	}
	tm := &trustMaterial{}
	var err error
	if tm.cas, err = cas(root.CertificateAuthorities); err != nil {
		return nil, err
	}
	if tm.tsas, err = cas(root.TimestampAuthorities); err != nil {
		return nil, err
	}
	if tm.tlogs, err = logs(root.Tlogs); err != nil {
		return nil, err
	}
	if tm.ctlogs, err = logs(root.Ctlogs); err != nil {
		return nil, err
	}
	if len(tm.cas) == 0 {
		return nil, errors.New("no certificate authorities")
	}
	return tm, nil
}
