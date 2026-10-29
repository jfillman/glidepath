package evidence

// A port of the part of @sigstore/bundle 5.0.0 (bundleFromJSON and its validation) and
// @sigstore/verify 4.1.2 (Verifier.verify) that Tower's provenance check uses, with the same checks
// in the same order and the same error messages. The messages are part of the answer: Tower shows
// verificationError and derives verificationCode from it (classifyVerification), so a shadowed
// call only agrees if the failure reads the same. sigstore-go verifies the same things but fails in
// its own words and on its own policy (it wants an observer timestamp, and has no notion of
// Tower's "Rekor key unavailable: verify without the log" degrade), so it is not used here.
//
// Two trust materials go through it. The platform's (Tower's buildTrustMaterial): one certificate
// authority (the Fulcio root), at most one transparency log (Rekor, valid for all time), no
// timestamp authorities, no CT logs, with the Verifier options {tlogThreshold: 0|1,
// ctlogThreshold: 0}. And public Sigstore's (promotion.go, Tower's promotionEvidence.ts): the
// embedded public-good trusted root, with its CAs, logs, CT logs and timestamp authority, the
// options {tlogThreshold: 1, ctlogThreshold: 1} and an identity policy. No bare public keys in
// either; timestampThreshold stays at its default of 1.

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"math"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/glidepath/api/internal/jsval"
)

const (
	bundleV01 = "application/vnd.dev.sigstore.bundle+json;version=0.1"
	bundleV02 = "application/vnd.dev.sigstore.bundle+json;version=0.2"
	// tlogBaseURL is the base URL Tower gives its Rekor authority; a checkpoint's signer name must
	// be a substring of it.
	tlogBaseURL = "http://rekor-server.rekor-system.svc"
	// endOfTime is JavaScript's largest Date, in milliseconds: the end of the tlog key's validity.
	endOfTime = 8640000000000000.0
)

// trustMaterial is @sigstore/verify's TrustMaterial. Times are milliseconds, as JavaScript Dates.
type trustMaterial struct {
	cas    []certAuthority
	tlogs  []tlogAuthority
	ctlogs []tlogAuthority
	tsas   []certAuthority // timestamp authorities
}

type certAuthority struct {
	chain      []*x509.Certificate
	start, end float64
}

func (c certAuthority) validAt(at float64) bool { return c.start <= at && at <= c.end }

type tlogAuthority struct {
	logID      []byte // the platform's: SHA-256 of the key's DER, computed, never taken from a bundle
	baseURL    string
	key        crypto.PublicKey
	start, end float64
}

func (l tlogAuthority) validAt(at float64) bool { return l.start <= at && at <= l.end }

// --- the bundle, as @sigstore/protobuf-specs' fromJSON builds it ---

type bundle struct {
	mediaType string
	vm        *verificationMaterial
	dsse      *envelope         // content: dsseEnvelope
	msg       *messageSignature // content: messageSignature (checked first, as fromJSON does)
}

type verificationMaterial struct {
	content     string   // "publicKey", "x509CertificateChain", "certificate" or ""
	certs       [][]byte // the chain's certificates, or the one certificate
	tlogEntries []*tlogEntry
	tsas        [][]byte // rfc3161Timestamps' signedTimestamp
}

type tlogEntry struct {
	logIndex       string
	logID          []byte // nil: logId unset
	kindVersion    *kindVersion
	integratedTime string
	promise        []byte // nil: inclusionPromise unset
	proof          *inclusionProof
	body           []byte
}

type kindVersion struct{ kind, version string }

type inclusionProof struct {
	logIndex   string
	hashes     [][]byte
	checkpoint *string
}

type envelope struct {
	payload     []byte
	payloadType string
	sigs        [][]byte
}

type messageSignature struct {
	hasDigest bool
	algorithm int
	digest    []byte
	signature []byte
}

func isSet(v any) bool { return !jsval.Nullish(v) }

func str(v any, def string) string {
	if isSet(v) {
		return jsval.String(v)
	}
	return def
}

func b64(v any) []byte {
	if isSet(v) {
		return jsval.Base64(jsval.String(v))
	}
	return []byte{}
}

func arr(v any) []any {
	a, _ := v.([]any)
	return a
}

var hashAlgorithms = map[string]int{
	"0": 0, "HASH_ALGORITHM_UNSPECIFIED": 0, "1": 1, "SHA2_256": 1, "2": 2, "SHA2_384": 2,
	"3": 3, "SHA2_512": 3, "4": 4, "SHA3_256": 4, "5": 5, "SHA3_384": 5,
}

// bundleFromJSON is @sigstore/bundle's: decode, then validate by media type.
func bundleFromJSON(obj any) (b *bundle, err error) {
	b = &bundle{mediaType: str(jsval.Get(obj, "mediaType"), "")}
	if vm := jsval.Get(obj, "verificationMaterial"); isSet(vm) {
		b.vm = &verificationMaterial{}
		switch {
		case isSet(jsval.Get(vm, "publicKey")):
			b.vm.content = "publicKey"
		case isSet(jsval.Get(vm, "x509CertificateChain")):
			b.vm.content = "x509CertificateChain"
			b.vm.certs = [][]byte{}
			for _, c := range arr(jsval.Path(vm, "x509CertificateChain", "certificates")) {
				b.vm.certs = append(b.vm.certs, b64(jsval.Get(c, "rawBytes")))
			}
		case isSet(jsval.Get(vm, "certificate")):
			b.vm.content = "certificate"
			b.vm.certs = [][]byte{b64(jsval.Path(vm, "certificate", "rawBytes"))}
		}
		for _, e := range arr(jsval.Get(vm, "tlogEntries")) {
			t := &tlogEntry{
				logIndex:       str(jsval.Get(e, "logIndex"), "0"),
				integratedTime: str(jsval.Get(e, "integratedTime"), "0"),
				body:           b64(jsval.Get(e, "canonicalizedBody")),
			}
			if id := jsval.Get(e, "logId"); isSet(id) {
				t.logID = b64(jsval.Get(id, "keyId"))
			}
			if kv := jsval.Get(e, "kindVersion"); isSet(kv) {
				t.kindVersion = &kindVersion{str(jsval.Get(kv, "kind"), ""), str(jsval.Get(kv, "version"), "")}
			}
			if p := jsval.Get(e, "inclusionPromise"); isSet(p) {
				t.promise = b64(jsval.Get(p, "signedEntryTimestamp"))
			}
			if p := jsval.Get(e, "inclusionProof"); isSet(p) {
				t.proof = &inclusionProof{logIndex: str(jsval.Get(p, "logIndex"), "0"), hashes: [][]byte{}}
				for _, h := range arr(jsval.Get(p, "hashes")) {
					t.proof.hashes = append(t.proof.hashes, b64(h))
				}
				if cp := jsval.Get(p, "checkpoint"); isSet(cp) {
					env := str(jsval.Get(cp, "envelope"), "")
					t.proof.checkpoint = &env
				}
			}
			b.vm.tlogEntries = append(b.vm.tlogEntries, t)
		}
		if tvd := jsval.Get(vm, "timestampVerificationData"); isSet(tvd) {
			for _, ts := range arr(jsval.Get(tvd, "rfc3161Timestamps")) {
				b.vm.tsas = append(b.vm.tsas, b64(jsval.Get(ts, "signedTimestamp")))
			}
		}
	}
	if ms := jsval.Get(obj, "messageSignature"); isSet(ms) {
		b.msg = &messageSignature{signature: b64(jsval.Get(ms, "signature"))}
		if md := jsval.Get(ms, "messageDigest"); isSet(md) {
			b.msg.hasDigest = true
			b.msg.digest = b64(jsval.Get(md, "digest"))
			if alg := jsval.Get(md, "algorithm"); isSet(alg) {
				n, ok := hashAlgorithms[jsval.String(alg)]
				if !ok {
					return nil, fmt.Errorf("Unrecognized enum value %s for enum HashAlgorithm", jsval.String(alg))
				}
				b.msg.algorithm = n
			}
		}
	} else if env := jsval.Get(obj, "dsseEnvelope"); isSet(env) {
		b.dsse = &envelope{payload: b64(jsval.Get(env, "payload")), payloadType: str(jsval.Get(env, "payloadType"), ""), sigs: [][]byte{}}
		for _, s := range arr(jsval.Get(env, "signatures")) {
			b.dsse.sigs = append(b.dsse.sigs, b64(jsval.Get(s, "sig")))
		}
	}

	var invalid []string
	invalid = append(invalid, validateBase(b)...)
	msg := "invalid bundle"
	switch b.mediaType {
	case bundleV01:
		msg = "invalid v0.1 bundle"
		for i, t := range tlogEntries(b) {
			if t.promise == nil {
				invalid = append(invalid, fmt.Sprintf("verificationMaterial.tlogEntries[%d].inclusionPromise", i))
			}
		}
	case bundleV02:
		msg = "invalid v0.2 bundle"
		invalid = append(invalid, validateProof(b)...)
	default:
		invalid = append(invalid, validateProof(b)...)
		if b.vm != nil && b.vm.content == "x509CertificateChain" {
			invalid = append(invalid, "verificationMaterial.content.$case")
		}
	}
	if len(invalid) > 0 {
		return nil, errors.New(msg)
	}
	return b, nil
}

func tlogEntries(b *bundle) []*tlogEntry {
	if b.vm == nil {
		return nil
	}
	return b.vm.tlogEntries
}

var mediaTypeRe = regexp.MustCompile(`^application/vnd\.dev\.sigstore\.bundle\+json;version=\d\.\d|^application/vnd\.dev\.sigstore\.bundle\.v\d\.\d\+json`)

func validateBase(b *bundle) []string {
	var bad []string
	if !mediaTypeRe.MatchString(b.mediaType) {
		bad = append(bad, "mediaType")
	}
	switch {
	case b.msg != nil:
		if !b.msg.hasDigest || len(b.msg.digest) == 0 {
			bad = append(bad, "content.messageSignature.messageDigest")
		}
		if len(b.msg.signature) == 0 {
			bad = append(bad, "content.messageSignature.signature")
		}
	case b.dsse != nil:
		if len(b.dsse.payload) == 0 {
			bad = append(bad, "content.dsseEnvelope.payload")
		}
		if len(b.dsse.sigs) != 1 || len(b.dsse.sigs[0]) == 0 {
			bad = append(bad, "content.dsseEnvelope.signatures")
		}
	default:
		bad = append(bad, "content")
	}
	if b.vm == nil {
		return append(bad, "verificationMaterial")
	}
	switch b.vm.content {
	case "":
		bad = append(bad, "verificationMaterial.content")
	case "x509CertificateChain", "certificate":
		if len(b.vm.certs) == 0 {
			bad = append(bad, "verificationMaterial.content.x509CertificateChain.certificates")
		}
		for _, c := range b.vm.certs {
			if len(c) == 0 {
				bad = append(bad, "verificationMaterial.content.certificate.rawBytes")
			}
		}
	}
	for _, t := range b.vm.tlogEntries {
		if t.logID == nil || t.kindVersion == nil {
			bad = append(bad, "verificationMaterial.tlogEntries")
		}
	}
	return bad
}

func validateProof(b *bundle) []string {
	var bad []string
	for _, t := range tlogEntries(b) {
		if t.proof == nil || t.proof.checkpoint == nil {
			bad = append(bad, "verificationMaterial.tlogEntries.inclusionProof")
		}
	}
	return bad
}

// --- the signed entity (toSignedEntity) ---

// content is what was signed: a DSSE envelope or a message signature over an artifact.
type content interface {
	signature() []byte
	compareDigest(d []byte) bool
	compareSignedDigest(d []byte) bool
	verify(key crypto.PublicKey) bool
}

type dsseContent struct{ env *envelope }

func (c dsseContent) signature() []byte {
	if len(c.env.sigs) > 0 {
		return c.env.sigs[0]
	}
	return []byte{}
}
func (c dsseContent) pae() []byte {
	return []byte(fmt.Sprintf("DSSEv1 %d %s %d %s", len(c.env.payloadType), c.env.payloadType, len(c.env.payload), c.env.payload))
}
func (c dsseContent) compareDigest(d []byte) bool {
	h := sha256.Sum256(c.env.payload)
	return bytes.Equal(d, h[:])
}
func (c dsseContent) compareSignedDigest(d []byte) bool {
	h := sha256.Sum256(c.pae())
	return bytes.Equal(d, h[:])
}
func (c dsseContent) verify(key crypto.PublicKey) bool {
	return verifySig(c.pae(), key, c.signature(), "")
}

type messageContent struct {
	ms       *messageSignature
	artifact []byte
}

func (c messageContent) signature() []byte                 { return c.ms.signature }
func (c messageContent) compareDigest(d []byte) bool       { return bytes.Equal(d, c.ms.digest) }
func (c messageContent) compareSignedDigest(d []byte) bool { return c.compareDigest(d) }
func (c messageContent) verify(key crypto.PublicKey) bool {
	alg := map[int]string{0: "sha256", 1: "sha256", 2: "sha384", 3: "sha512", 4: "sha3-256", 5: "sha3-384"}[c.ms.algorithm]
	return verifySig(c.artifact, key, c.ms.signature, alg)
}

type signedEntity struct {
	content     content
	publicKey   bool // a bare-key bundle: Tower trusts no bare keys
	leaf        *x509.Certificate
	tlogEntries []*tlogEntry
	timestamps  []*tlogEntry // tlog entries that carry a time
	tsas        []*rfc3161Timestamp
}

func toSignedEntity(b *bundle, artifact []byte) (*signedEntity, error) {
	e := &signedEntity{tlogEntries: b.vm.tlogEntries}
	for _, t := range b.vm.tlogEntries {
		if t.integratedTime != "" && t.integratedTime != "0" {
			e.timestamps = append(e.timestamps, t)
		}
	}
	for _, der := range b.vm.tsas {
		ts, err := parseRFC3161(der)
		if err != nil {
			return nil, err
		}
		e.tsas = append(e.tsas, ts)
	}
	if b.msg != nil {
		e.content = messageContent{b.msg, artifact}
	} else {
		e.content = dsseContent{b.dsse}
	}
	switch b.vm.content {
	case "publicKey":
		e.publicKey = true
	default:
		cert, err := parseCert(b.vm.certs[0])
		if err != nil {
			return nil, err
		}
		e.leaf = cert
	}
	return e, nil
}

// --- Verifier.verify ---

type verifyError string

func (e verifyError) Error() string { return string(e) }

// verifyBundle is Tower's `new Verifier(trustMaterial, {tlogThreshold, ctlogThreshold: 0}).verify(toSignedEntity(bundle, artifact))`.
func (tm *trustMaterial) verifyBundle(b *bundle, artifact []byte, tlogThreshold int) error {
	return tm.verify(b, artifact, verifyOptions{tlogThreshold: tlogThreshold})
}

type verifyOptions struct {
	tlogThreshold, ctlogThreshold int
	policy                        *identityPolicy // nil: none
}

// verify is `new Verifier(trustMaterial, opts).verify(toSignedEntity(bundle, artifact), policy)`.
func (tm *trustMaterial) verify(b *bundle, artifact []byte, opts verifyOptions) error {
	e, err := toSignedEntity(b, artifact)
	if err != nil {
		return err
	}
	timestamps, err := tm.verifyTimestamps(e)
	if err != nil {
		return err
	}
	if e.publicKey {
		return errors.New("no bare public keys trusted - certificate-based signing only")
	}
	if err := tm.verifyCertificate(e.leaf, timestamps, opts.ctlogThreshold); err != nil {
		return err
	}
	if err := tm.verifyTLogs(e, opts.tlogThreshold); err != nil {
		return err
	}
	if !e.content.verify(e.leaf.PublicKey) {
		return verifyError("signature verification failed")
	}
	if opts.policy != nil {
		return opts.policy.verify(e.leaf)
	}
	return nil
}

type timestamp struct {
	tsa   bool // a timestamp authority's, else a log's
	logID []byte
	ms    float64
}

func (tm *trustMaterial) verifyTimestamps(e *signedEntity) ([]float64, error) {
	var ts []timestamp
	for _, t := range e.timestamps {
		if t.promise != nil { // only an entry with an inclusion promise gives a time
			ts = append(ts, timestamp{false, t.logID, jsval.Number(t.integratedTime) * 1000})
		}
	}
	for _, t := range e.tsas {
		at, err := tm.verifyRFC3161(t, e.content.signature())
		if err != nil {
			return nil, err
		}
		ts = append(ts, timestamp{true, t.signerSerial(), ms(at)})
	}
	for i := range ts {
		for j := i + 1; j < len(ts); j++ {
			if ts[i].tsa == ts[j].tsa && bytes.Equal(ts[i].logID, ts[j].logID) && (ts[i].ms == ts[j].ms || math.IsNaN(ts[i].ms) && math.IsNaN(ts[j].ms)) {
				return nil, verifyError("duplicate timestamp")
			}
		}
	}
	if len(ts) < 1 {
		return nil, verifyError(fmt.Sprintf("expected 1 timestamps, got %d", len(ts)))
	}
	out := make([]float64, len(ts))
	for i, t := range ts {
		out[i] = t.ms
	}
	return out, nil
}

func ms(t time.Time) float64 { return float64(t.UnixMilli()) }

// verifyCertificate is verifyCertificate plus Verifier.verifySigningKey's SCT checks: the chain at
// every timestamp, then the SCTs of the last path's leaf. An SCT that is present must verify even
// when ctlogThreshold is 0, so with no CT log trusted (the platform's) any SCT fails.
func (tm *trustMaterial) verifyCertificate(leaf *x509.Certificate, timestamps []float64, ctlogThreshold int) error {
	var path []*x509.Certificate
	for _, at := range timestamps {
		p, err := tm.verifyChain(at, leaf)
		if err != nil {
			return err
		}
		path = p
	}
	scts, err := tm.verifySCTs(path)
	if err != nil {
		return err
	}
	for i := range scts {
		for j := i + 1; j < len(scts); j++ {
			if bytes.Equal(scts[i], scts[j]) {
				return verifyError("duplicate SCT")
			}
		}
	}
	if len(scts) < ctlogThreshold {
		return verifyError(fmt.Sprintf("expected %d SCTs, got %d", ctlogThreshold, len(scts)))
	}
	return nil
}

// verifyChain is verifyCertificateChain: the first CA valid at the time whose chain verifies.
// Every failure reads the same.
func (tm *trustMaterial) verifyChain(at float64, leaf *x509.Certificate) ([]*x509.Certificate, error) {
	for _, ca := range tm.cas {
		if !ca.validAt(at) {
			continue
		}
		if path, err := chainVerify(ca.chain, leaf, at); err == nil {
			return path, nil
		}
	}
	return nil, verifyError("Failed to verify certificate chain")
}

// chainVerify is CertificateChainVerifier.verify: the shortest path from the leaf through the
// local certificates (the trusted ones and the leaf) that contains a trusted one, then the CA,
// name chaining, path length and validity checks.
func chainVerify(trusted []*x509.Certificate, leaf *x509.Certificate, at float64) ([]*x509.Certificate, error) {
	fail := errors.New("certificate chain does not verify")
	var local []*x509.Certificate
	for _, c := range append(append([]*x509.Certificate{}, trusted...), leaf) {
		dup := false
		for _, l := range local {
			if bytes.Equal(l.Raw, c.Raw) {
				dup = true
			}
		}
		if !dup {
			local = append(local, c)
		}
	}
	paths, err := buildPaths(leaf, local, 0)
	if err != nil {
		return nil, fail
	}
	isTrusted := func(c *x509.Certificate) bool {
		for _, t := range trusted {
			if bytes.Equal(t.Raw, c.Raw) {
				return true
			}
		}
		return false
	}
	var best []*x509.Certificate
	for _, p := range paths {
		has := false
		for _, c := range p {
			has = has || isTrusted(c)
		}
		if has && (best == nil || len(p) < len(best)) {
			best = p
		}
	}
	if best == nil {
		return nil, fail
	}
	path := append([]*x509.Certificate{leaf}, best...)
	path = path[:len(path)-1]
	for _, c := range path[1:] {
		if !isCA(c) {
			return nil, fail
		}
	}
	for i := len(path) - 2; i >= 0; i-- {
		if !bytes.Equal(path[i].RawIssuer, path[i+1].RawSubject) {
			return nil, fail
		}
	}
	for i, c := range path {
		if c.BasicConstraintsValid && c.IsCA && (c.MaxPathLen > 0 || c.MaxPathLenZero) && c.MaxPathLen < i-1 {
			return nil, fail
		}
	}
	for _, c := range path {
		if !(ms(c.NotBefore) <= at && at <= ms(c.NotAfter)) {
			return nil, fail
		}
	}
	return path, nil
}

func buildPaths(c *x509.Certificate, local []*x509.Certificate, depth int) ([][]*x509.Certificate, error) {
	if depth > 16 {
		return nil, errors.New("certificate path too deep")
	}
	issuers := findIssuer(c, local)
	if len(issuers) == 0 {
		return nil, errors.New("no valid certificate path found")
	}
	var paths [][]*x509.Certificate
	for _, iss := range issuers {
		if bytes.Equal(iss.Raw, c.Raw) {
			paths = append(paths, []*x509.Certificate{c})
			continue
		}
		sub, err := buildPaths(iss, local, depth+1)
		if err != nil {
			return nil, err
		}
		for _, s := range sub {
			paths = append(paths, append([]*x509.Certificate{iss}, s...))
		}
	}
	return paths, nil
}

func findIssuer(c *x509.Certificate, local []*x509.Certificate) []*x509.Certificate {
	if bytes.Equal(c.RawSubject, c.RawIssuer) && checkSignedBy(c, c) {
		return []*x509.Certificate{c}
	}
	aki := c.AuthorityKeyId
	var out []*x509.Certificate
	for _, p := range local {
		if len(aki) > 0 && p.SubjectKeyId != nil {
			if bytes.Equal(p.SubjectKeyId, aki) && checkSignedBy(c, p) {
				out = append(out, p)
			}
			continue
		}
		if bytes.Equal(p.RawSubject, c.RawIssuer) && checkSignedBy(c, p) {
			out = append(out, p)
		}
	}
	return out
}

// checkSignedBy checks only the signature, as @sigstore/core's X509Certificate.verify does; the
// CA and path checks are verifyChain's.
func checkSignedBy(c, issuer *x509.Certificate) bool {
	return issuer.CheckSignature(c.SignatureAlgorithm, c.RawTBSCertificate, c.Signature) == nil
}

var oidKeyUsage = asn1.ObjectIdentifier{2, 5, 29, 15}

func isCA(c *x509.Certificate) bool {
	ca := c.BasicConstraintsValid && c.IsCA
	for _, ext := range c.Extensions {
		if ext.Id.Equal(oidKeyUsage) {
			return ca && c.KeyUsage&x509.KeyUsageCertSign != 0
		}
	}
	return ca
}

func (tm *trustMaterial) verifyTLogs(e *signedEntity, threshold int) error {
	type id struct{ logID, logIndex string }
	var ids []id
	for _, t := range e.tlogEntries {
		if err := tm.verifyInclusion(t); err != nil {
			return err
		}
		if err := verifyTLogBody(t, e.content); err != nil {
			return err
		}
		ids = append(ids, id{string(t.logID), t.logIndex})
	}
	for i := range ids {
		for j := i + 1; j < len(ids); j++ {
			if ids[i] == ids[j] {
				return verifyError("duplicate tlog entry")
			}
		}
	}
	if len(ids) < threshold {
		return verifyError(fmt.Sprintf("expected %d tlog entries, got %d", threshold, len(ids)))
	}
	return nil
}

func (tm *trustMaterial) verifyInclusion(t *tlogEntry) error {
	verified := false
	if t.promise != nil {
		if err := tm.verifySET(t); err != nil {
			return err
		}
		verified = true
	}
	if t.proof != nil {
		if t.proof.checkpoint == nil {
			return errors.New("Cannot read properties of undefined (reading 'envelope')")
		}
		size, root, err := tm.verifyCheckpoint(*t.proof.checkpoint)
		if err != nil {
			return err
		}
		if err := verifyMerkle(t, size, root); err != nil {
			return err
		}
		verified = true
	}
	if !verified {
		return verifyError("inclusion could not be verified")
	}
	return nil
}

// verifySET checks Rekor's signature over the canonical {body, integratedTime, logID, logIndex}.
func (tm *trustMaterial) verifySET(t *tlogEntry) error {
	at := jsval.Number(t.integratedTime) * 1000
	data := fmt.Sprintf(`{"body":%q,"integratedTime":%s,"logID":%q,"logIndex":%s}`,
		base64.StdEncoding.EncodeToString(t.body), jsonNumber(jsval.Number(t.integratedTime)),
		hex.EncodeToString(t.logID), jsonNumber(jsval.Number(t.logIndex)))
	for _, log := range tm.tlogs {
		if !bytes.Equal(log.logID, t.logID) || !log.validAt(at) {
			continue
		}
		if verifySig([]byte(data), log.key, t.promise, "") {
			return nil
		}
	}
	return verifyError("inclusion promise could not be verified")
}

// jsonNumber is JSON.stringify of a number: null for NaN and the infinities.
func jsonNumber(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "null"
	}
	return jsval.NumberString(f)
}

var checkpointSig = regexp.MustCompile("— (\\S+) (\\S+)\n")

func (tm *trustMaterial) verifyCheckpoint(env string) (*big.Int, []byte, error) {
	fail := func(m string) (*big.Int, []byte, error) { return nil, nil, verifyError(m) }
	split := strings.Index(env, "\n\n")
	if split < 0 {
		return fail("missing checkpoint separator")
	}
	note, data := env[:split+1], env[split+2:]
	type sig struct {
		name      string
		hint, sig []byte
	}
	var sigs []sig
	for _, m := range checkpointSig.FindAllStringSubmatch(data, -1) {
		raw := jsval.Base64(m[2])
		if len(raw) < 5 {
			return fail("malformed checkpoint signature")
		}
		sigs = append(sigs, sig{m[1], raw[:4], raw[4:]})
	}
	if len(sigs) == 0 {
		return fail("no signatures found in checkpoint")
	}
	lines := strings.Split(strings.TrimRightFunc(note, isJSSpace), "\n")
	if len(lines) < 3 {
		return fail("too few lines in checkpoint header")
	}
	size, ok := bigInt(lines[1])
	if !ok {
		return fail("invalid checkpoint log size")
	}
	rootHash := jsval.Base64(lines[2])
	for _, s := range sigs {
		for _, log := range tm.tlogs {
			if len(log.logID) >= 4 && bytes.Equal(log.logID[:4], s.hint) && strings.Contains(log.baseURL, s.name) {
				if verifySig([]byte(note), log.key, s.sig, "") {
					return size, rootHash, nil
				}
				break
			}
		}
	}
	return fail("invalid checkpoint signature")
}

func isJSSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f' || r == 0xa0 || r == 0xfeff ||
		r == 0x1680 || (r >= 0x2000 && r <= 0x200a) || r == 0x2028 || r == 0x2029 || r == 0x202f || r == 0x205f || r == 0x3000
}

// bigInt is JavaScript's BigInt(string).
func bigInt(s string) (*big.Int, bool) {
	s = strings.TrimFunc(s, isJSSpace)
	if s == "" {
		return new(big.Int), true
	}
	n := new(big.Int)
	if len(s) > 2 && s[0] == '0' && strings.ContainsRune("xXoObB", rune(s[1])) {
		base := map[byte]int{'x': 16, 'X': 16, 'o': 8, 'O': 8, 'b': 2, 'B': 2}[s[1]]
		_, ok := n.SetString(s[2:], base)
		return n, ok && !strings.ContainsAny(s[2:], "+-_")
	}
	digits := strings.TrimLeft(s, "+-")
	if len(s)-len(digits) > 1 || digits == "" || strings.ContainsAny(digits, "_") {
		return nil, false
	}
	_, ok := n.SetString(s, 10)
	return n, ok
}

func verifyMerkle(t *tlogEntry, size *big.Int, root []byte) error {
	index, ok := bigInt(t.proof.logIndex)
	if !ok {
		return verifyError("invalid inclusion proof log index")
	}
	if index.Sign() < 0 || index.Cmp(size) >= 0 {
		return verifyError("invalid index: " + index.String())
	}
	last := new(big.Int).Sub(size, big.NewInt(1))
	inner := new(big.Int).Xor(index, last).BitLen()
	border := 0
	for _, w := range new(big.Int).Rsh(index, uint(inner)).Bits() {
		for ; w != 0; w &= w - 1 {
			border++
		}
	}
	if len(t.proof.hashes) != inner+border {
		return verifyError("invalid hash count")
	}
	node := func(l, r []byte) []byte {
		h := sha256.New()
		h.Write([]byte{1})
		h.Write(l)
		h.Write(r)
		return h.Sum(nil)
	}
	leaf := sha256.Sum256(append([]byte{0}, t.body...))
	acc := leaf[:]
	for i, h := range t.proof.hashes[:inner] {
		if index.Bit(i) == 1 {
			acc = node(h, acc)
		} else {
			acc = node(acc, h)
		}
	}
	for _, h := range t.proof.hashes[inner:] {
		acc = node(h, acc)
	}
	if !bytes.Equal(acc, root) {
		return verifyError("calculated root hash does not match inclusion proof")
	}
	return nil
}

// verifyTLogBody checks the log entry is about this signature and this content.
func verifyTLogBody(t *tlogEntry, c content) error {
	kind, version := t.kindVersion.kind, t.kindVersion.version
	body, err := jsval.Parse(t.body)
	if err != nil {
		return verifyError("invalid canonicalized body")
	}
	bk, bv := jsval.Get(body, "kind"), jsval.Get(body, "apiVersion")
	if bk != any(kind) || bv != any(version) {
		return verifyError(fmt.Sprintf("kind/version mismatch - expected: %s/%s, received: %s/%s", kind, version, jsval.String(bk), jsval.String(bv)))
	}
	spec := jsval.Get(body, "spec")
	switch kind {
	case "dsse":
		if version != "0.0.1" {
			return rekorV2Unsupported(body)
		}
		if err := need(spec, "signatures"); err != nil {
			return err
		}
		sigs := arr(jsval.Get(spec, "signatures"))
		if len(sigs) != 1 {
			return verifyError("signature count mismatch")
		}
		if !bytes.Equal(c.signature(), jsval.Base64(jsval.String(jsval.Get(sigs[0], "signature")))) {
			return verifyError("tlog entry signature mismatch")
		}
		if !c.compareDigest(jsval.Hex(orEmpty(jsval.Path(spec, "payloadHash", "value")))) {
			return verifyError("DSSE payload hash mismatch")
		}
		return nil
	case "intoto":
		if version != "0.0.2" {
			return verifyError("unsupported intoto version: " + version)
		}
		if err := need(spec, "content"); err != nil {
			return err
		}
		if err := need(jsval.Get(spec, "content"), "envelope"); err != nil {
			return err
		}
		if err := need(jsval.Path(spec, "content", "envelope"), "signatures"); err != nil {
			return err
		}
		sigs := arr(jsval.Path(spec, "content", "envelope", "signatures"))
		if len(sigs) != 1 {
			return verifyError("signature count mismatch")
		}
		// The signature is base64 twice over in an intoto entry.
		once := string(jsval.Base64(jsval.String(jsval.Get(sigs[0], "sig"))))
		if !bytes.Equal(c.signature(), jsval.Base64(once)) {
			return verifyError("tlog entry signature mismatch")
		}
		if !c.compareDigest(jsval.Hex(orEmpty(jsval.Path(spec, "content", "payloadHash", "value")))) {
			return verifyError("DSSE payload hash mismatch")
		}
		return nil
	case "hashedrekord":
		if version != "0.0.1" {
			return rekorV2Unsupported(body)
		}
		if err := need(spec, "signature"); err != nil {
			return err
		}
		if err := need(jsval.Get(spec, "signature"), "content"); err != nil {
			return err
		}
		if !bytes.Equal(c.signature(), jsval.Base64(orEmpty(jsval.Path(spec, "signature", "content")))) {
			return verifyError("signature mismatch")
		}
		if err := need(spec, "data"); err != nil {
			return err
		}
		if err := need(jsval.Get(spec, "data"), "hash"); err != nil {
			return err
		}
		if !c.compareSignedDigest(jsval.Hex(orEmpty(jsval.Path(spec, "data", "hash", "value")))) {
			return verifyError("digest mismatch")
		}
		return nil
	}
	return verifyError("unsupported kind: " + kind)
}

// rekorV2Unsupported: Rekor v2 entries (dsse/hashedrekord 0.0.2). Hangar's Rekor is v1; this
// answers as @sigstore/verify does for a v2 body without the spec it needs.
func rekorV2Unsupported(body any) error {
	if !isSet(jsval.Path(body, "spec", "hashedRekordV002")) && !isSet(jsval.Path(body, "spec", "dsseV002")) {
		return verifyError("missing dsse spec")
	}
	return verifyError("unsupported version: rekor v2 entries are not verified by glidepath-api")
}

// orEmpty is `x || ”`.
func orEmpty(v any) string {
	if jsval.Truthy(v) {
		return jsval.String(v)
	}
	return ""
}

// need is the TypeError JavaScript throws reading prop of null or undefined.
func need(v any, prop string) error {
	switch v {
	case nil:
		return fmt.Errorf("Cannot read properties of null (reading '%s')", prop)
	case jsval.Undefined:
		return fmt.Errorf("Cannot read properties of undefined (reading '%s')", prop)
	}
	return nil
}

// verifySig is @sigstore/core's crypto.verify: Node's crypto.verify(algorithm, data, key, sig),
// false on any failure. An empty algorithm is the key's default (SHA-256 for ECDSA and RSA).
func verifySig(data []byte, key crypto.PublicKey, sig []byte, alg string) bool {
	var h hash.Hash
	var ch crypto.Hash
	switch alg {
	case "", "sha256":
		h, ch = sha256.New(), crypto.SHA256
	case "sha384":
		h, ch = sha512.New384(), crypto.SHA384
	case "sha512":
		h, ch = sha512.New(), crypto.SHA512
	default:
		return false
	}
	switch k := key.(type) {
	case ed25519.PublicKey:
		return alg == "" && ed25519.Verify(k, data, sig)
	case *ecdsa.PublicKey:
		h.Write(data)
		return ecdsa.VerifyASN1(k, h.Sum(nil), sig)
	case *rsa.PublicKey:
		h.Write(data)
		return rsa.VerifyPKCS1v15(k, ch, h.Sum(nil), sig) == nil
	}
	return false
}
