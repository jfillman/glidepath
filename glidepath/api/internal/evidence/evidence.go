// Package evidence reads an image's supply-chain evidence from GHCR: every signature and
// attestation on it, each re-verified against the cluster's own Fulcio root and Rekor.
//
// Ported from Tower's backend (backstage packages/backend/src/glidepathProvenance.ts:
// readImageProvenance and the three fetchers under it, with rekorEntries.ts and ghcr.ts) with the
// same reads, the same checks in the same order, and the same answer, so Tower can shadow its own
// /provenance against GET /v1/apps/{app}/evidence field for field (docs/admin/api.md). Keep the two
// in step until Tower's copy is retired.
//
// Three places an attestation or signature can live, all read, none dropped:
//   - the legacy tag <repo>:sha256-<digest>.att: Tekton Chains' SLSA provenance. A DSSE envelope
//     per layer, its certificate and Rekor receipt in the layer's annotations.
//   - OCI 1.1 referrers (or GHCR's referrers-tag fallback, <repo>:sha256-<digest>): cosign 3's
//     attestations (the SBOM). Each a complete sigstore bundle.
//   - the classic signature tag <repo>:sha256-<digest>.sig: a simple-signing payload signed as is.
//
// Every receipt is re-read from Rekor by log index (rekor.go), so it is signed by Rekor's current
// key; a body that differs from Rekor's at that index never verifies.
package evidence

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"unicode/utf16"

	"github.com/glidepath/api/internal/jsval"

	"k8s.io/client-go/kubernetes"
)

const sigstoreBundleArtifactPrefix = "application/vnd.dev.sigstore.bundle"

// Provenance is the answer: the image's digest and everything on it, in Tower's order (legacy
// attestations, then new-format ones, then signatures).
type Provenance struct {
	Digest       string       `json:"digest"`
	Attestations []*jsval.Obj `json:"attestations"`
}

// Reader reads evidence. Credentials is read once per request, for that request only.
type Reader struct {
	CS          kubernetes.Interface
	Credentials CredentialSource
	RegistryURL string // https://ghcr.io
	RekorURL    string // http://rekor-server.rekor-system.svc.cluster.local
	HTTP        *http.Client
	Log         *slog.Logger
	// Promotion verifies Flight promotion attestations (promotion.go); nil: they show, unverified.
	Promotion *PromotionTrust

	once  sync.Once
	trust *trustSource
}

func (r *Reader) init() {
	r.once.Do(func() {
		r.trust = &trustSource{cs: r.CS, rekor: httpGetter(r.HTTP, r.RekorURL)}
	})
}

// Read is Tower's readImageProvenance for an image of the app whose -cicd namespace is namespace.
func (r *Reader) Read(ctx context.Context, namespace, image string) (*Provenance, error) {
	r.init()
	repo, ref, err := ParseImageRef(image)
	if err != nil {
		return nil, err
	}
	reg := &registry{base: r.RegistryURL, http: r.HTTP}
	token, err := func() (string, error) {
		// The credential lives only inside this closure.
		auth, err := r.Credentials(ctx, namespace)
		if err != nil {
			return "", err
		}
		return reg.pullToken(ctx, repo, auth)
	}()
	if err != nil {
		return nil, err
	}
	digest := ref
	if !strings.HasPrefix(ref, "sha256:") {
		if digest, err = reg.resolveDigest(ctx, repo, ref, token); err != nil {
			return nil, err
		}
	}

	// A root-cert failure doesn't fail the request: everything still decodes and displays, marked
	// unverified with why. A Rekor key failure is softer still: verification goes on without the log.
	var tm *trustMaterial
	var tmErr *string
	if root, err := r.trust.fulcioRoot(ctx); err != nil {
		tmErr = ptr("could not load Fulcio trust root: " + err.Error())
	} else {
		rekorPem, err := r.trust.rekorPublicKey(ctx)
		if err != nil {
			r.Log.Warn("could not load Rekor public key, transparency-log verification unavailable this request", "error", err.Error())
			rekorPem = ""
		}
		if tm, err = buildTrustMaterial(root, rekorPem); err != nil {
			tmErr = ptr("could not load Fulcio trust root: " + err.Error())
		}
	}
	if tmErr != nil {
		r.Log.Warn(*tmErr)
	}

	f := &fetcher{reg: reg, repo: repo, digest: digest, digestHex: strings.Replace(digest, "sha256:", "", 1), token: token,
		tm: tm, tmErr: tmErr, rekor: NewRekorLookup(httpGetter(r.HTTP, r.RekorURL)), promotion: r.Promotion}
	var legacy, newFormat []attestation
	var legacyErr, newErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); legacy, legacyErr = f.legacy(ctx) }()
	go func() { defer wg.Done(); newFormat, newErr = f.newFormat(ctx) }()
	wg.Wait()
	if legacyErr != nil {
		return nil, legacyErr
	}
	if newErr != nil {
		return nil, newErr
	}
	// After the attestations: their log indexes are where to look for the signature's own entry (the
	// platform Rekor's; a promotion attestation's index is public Rekor's and is not a hint).
	var hints []float64
	for _, a := range append(append([]attestation{}, legacy...), newFormat...) {
		if a.logIndex != nil {
			hints = append(hints, *a.logIndex)
		}
	}
	signature, err := f.signature(ctx, hints)
	if err != nil {
		return nil, err
	}
	out := &Provenance{Digest: digest, Attestations: []*jsval.Obj{}}
	for _, list := range [][]attestation{legacy, newFormat, signature} {
		for _, a := range list {
			out.Attestations = append(out.Attestations, a.obj)
		}
	}
	return out, nil
}

func ptr[T any](v T) *T { return &v }

type attestation struct {
	obj      *jsval.Obj
	logIndex *float64 // the platform Rekor's transparencyLog.logIndex, when there is one
}

type fetcher struct {
	reg              *registry
	repo, digest     string
	digestHex, token string
	tm               *trustMaterial
	tmErr            *string
	rekor            RekorLookup
	promotion        *PromotionTrust
}

// verdict is what the checks concluded about one attestation.
type verdict struct {
	verified bool
	err      *string
}

const (
	ociManifest = "application/vnd.oci.image.manifest.v1+json"
	ociIndex    = "application/vnd.oci.image.index.v1+json"
)

// errSkip marks an attestation Tower's try/catch would have skipped (a TypeError on an odd shape).
type errSkip struct{ msg string }

func (e errSkip) Error() string { return e.msg }

// legacy reads <repo>:sha256-<digest>.att.
func (f *fetcher) legacy(ctx context.Context) ([]attestation, error) {
	status, body, err := f.reg.getBody(ctx, fmt.Sprintf("/v2/%s/manifests/sha256-%s.att", f.repo, f.digestHex), f.token, ociManifest)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return []attestation{}, nil
	}
	if !ok(status) {
		return nil, fmt.Errorf("attestation manifest request failed with %d", status)
	}
	manifest, err := jsval.Parse(body)
	if err != nil {
		return nil, fmt.Errorf("attestation manifest: %w", err)
	}
	out := []attestation{}
	for _, layer := range arr(jsval.Get(manifest, "layers")) {
		status, body, err := f.reg.getBody(ctx, fmt.Sprintf("/v2/%s/blobs/%s", f.repo, jsval.String(jsval.Get(layer, "digest"))), f.token, "")
		if err != nil {
			return nil, err
		}
		if !ok(status) {
			continue
		}
		// Each layer is a DSSE envelope; its payload is the in-toto statement. The certificate and
		// the Rekor receipt are annotations on the layer, not inside the blob.
		env, err := jsval.Parse(body)
		if err != nil {
			return nil, fmt.Errorf("attestation layer: %w", err) // outside Tower's try: it fails the request
		}
		payload := jsval.Get(env, "payload")
		if !jsval.Truthy(payload) {
			continue
		}
		a, err := f.legacyOne(ctx, layer, env, payload)
		if err != nil {
			continue // malformed: skipped, as Tower's catch does
		}
		out = append(out, a)
	}
	return out, nil
}

func (f *fetcher) legacyOne(ctx context.Context, layer, env, payload any) (attestation, error) {
	statement, err := jsval.Parse(jsval.Base64(jsval.String(payload)))
	if err != nil {
		return attestation{}, err
	}
	ann := jsval.Get(layer, "annotations")
	certPem := jsval.Get(ann, "dev.sigstore.cosign/certificate")
	sig := jsval.Get(jsval.Index(jsval.Get(env, "signatures"), 0), "sig")
	payloadType := jsval.Get(env, "payloadType")
	tlog := refreshTlogEntry(ctx, parseCosignTlogBundle(jsval.Get(ann, "dev.sigstore.cosign/bundle")), f.rekor)

	var v verdict
	switch {
	case f.tmErr != nil:
		v.err = f.tmErr
	case f.tm == nil:
		v.err = ptr("trust root unavailable")
	case !jsval.Truthy(certPem) || !jsval.Truthy(sig) || !jsval.Truthy(payloadType):
		v.err = ptr("attestation is missing its certificate or signature")
	case tlog.mismatch:
		v.err = ptr("Rekor's entry at this log index is not the one this attestation claims")
	default:
		if v, err = f.verifyAttestation(jsval.String(payload), jsval.String(payloadType), jsval.String(sig), jsval.String(certPem), statement, tlog.entry); err != nil {
			return attestation{}, err
		}
	}
	obj := spread(statement)
	return finish(obj, certPem, summarizeTlogEntry(tlog.entry), v, tlog.source, tlog.mismatch), nil
}

// finish sets the fields every attestation carries after its statement's, in Tower's order.
func finish(obj *jsval.Obj, certPem any, tl *jsval.Obj, v verdict, source TlogSource, mismatch bool) attestation {
	a := attestation{obj: obj}
	obj.Set("certificate", certificateValue(certPem))
	if tl != nil {
		obj.Set("transparencyLog", tl)
		n := tl.Get("logIndex").(jsval.Num)
		a.logIndex = ptr(float64(n))
	} else {
		obj.Set("transparencyLog", jsval.Undefined)
	}
	obj.Set("verified", v.verified)
	if v.err != nil {
		obj.Set("verificationError", *v.err)
	} else {
		obj.Set("verificationError", jsval.Undefined)
	}
	obj.Set("verificationCode", string(classifyVerification(v.verified, v.err, source, mismatch)))
	obj.Set("logSource", string(source))
	return a
}

// certificateValue is `certPem ? summarizeCertificate(certPem) : undefined`.
func certificateValue(certPem any) any {
	if !jsval.Truthy(certPem) {
		return jsval.Undefined
	}
	s, isString := certPem.(string)
	if !isString {
		return jsval.Undefined
	}
	if c := summarizeCertificate(s); c != nil {
		return c
	}
	return jsval.Undefined
}

// spread is {...statement}: an object's own keys, a string's characters by index, nothing else.
func spread(v any) *jsval.Obj {
	switch x := v.(type) {
	case *jsval.Obj:
		return x.Clone()
	case []any:
		o := jsval.NewObj()
		for i, e := range x {
			o.Set(fmt.Sprint(i), e)
		}
		return o
	case string:
		o := jsval.NewObj()
		for i, u := range utf16.Encode([]rune(x)) {
			o.Set(fmt.Sprint(i), string(utf16.Decode([]uint16{u})))
		}
		return o
	}
	return jsval.NewObj()
}

// parseCosignTlogBundle reads cosign's dev.sigstore.cosign/bundle annotation: its flat shape
// {SignedEntryTimestamp, Payload: {logIndex, logID, integratedTime, body}}. Absent or malformed is
// nil: attestations from before Rekor existed are an expected, displayable state.
func parseCosignTlogBundle(annotation any) *RawTlogEntry {
	if !jsval.Truthy(annotation) {
		return nil
	}
	b, err := jsval.Parse([]byte(jsval.String(annotation)))
	if err != nil {
		return nil
	}
	p := jsval.Get(b, "Payload")
	set := jsval.Get(b, "SignedEntryTimestamp")
	if !jsval.Truthy(p) || jsval.Get(p, "logIndex") == jsval.Undefined || !jsval.Truthy(jsval.Get(p, "logID")) ||
		!jsval.Truthy(jsval.Get(p, "body")) || !jsval.Truthy(set) {
		return nil
	}
	body := jsval.String(jsval.Get(p, "body"))
	kind, version, good := kindVersionOf(body)
	if !good {
		return nil
	}
	return &RawTlogEntry{
		LogIndex:             jsval.String(jsval.Get(p, "logIndex")),
		LogIDHex:             jsval.String(jsval.Get(p, "logID")),
		IntegratedTime:       jsval.String(jsval.Or(jsval.Get(p, "integratedTime"), "")),
		SignedEntryTimestamp: jsval.String(set),
		CanonicalizedBody:    body,
		Kind:                 kind,
		Version:              version,
	}
}

// summarizeTlogEntry is the transparencyLog Tower shows: logIndex, logId, integratedTime.
func summarizeTlogEntry(raw *RawTlogEntry) *jsval.Obj {
	if raw == nil {
		return nil
	}
	o := jsval.NewObj().Set("logIndex", jsval.Num(jsval.Number(raw.LogIndex))).Set("logId", raw.LogIDHex)
	if raw.IntegratedTime != "" {
		o.Set("integratedTime", jsval.Num(jsval.Number(raw.IntegratedTime)))
	} else {
		o.Set("integratedTime", jsval.Undefined)
	}
	return o
}

// subjectMatches is statement.subject?.some(s => s.digest?.sha256 === digestHex). A shape that
// would throw in TypeScript returns errSkip.
func subjectMatches(statement any, digestHex string) (bool, error) {
	if jsval.Nullish(statement) {
		return false, errSkip{"statement is null"}
	}
	subj := jsval.Get(statement, "subject")
	if jsval.Nullish(subj) {
		return false, nil
	}
	list, isList := subj.([]any)
	if !isList {
		return false, errSkip{"statement.subject?.some is not a function"}
	}
	for _, s := range list {
		if jsval.Nullish(s) {
			return false, errSkip{"subject entry is null"}
		}
		if jsval.Path(s, "digest", "sha256") == any(digestHex) {
			return true, nil
		}
	}
	return false, nil
}

var pemArmor = regexp.MustCompile(`-----(BEGIN|END) CERTIFICATE-----`)
var whitespace = regexp.MustCompile(`\s+`)

func pemToBase64Der(p string) string {
	return whitespace.ReplaceAllString(pemArmor.ReplaceAllString(p, ""), "")
}

// derBase64ToPem wraps a bundle's bare base64 DER certificate as PEM.
func derBase64ToPem(der string) string {
	var lines []string
	r := []rune(der)
	for len(r) > 64 {
		lines = append(lines, string(r[:64]))
		r = r[64:]
	}
	lines = append(lines, string(r))
	return "-----BEGIN CERTIFICATE-----\n" + strings.Join(lines, "\n") + "\n-----END CERTIFICATE-----"
}

// v01TlogEntry is the tlog entry Tower's rebuilt v0.1 bundles carry: the receipt (a SET), no proof.
func v01TlogEntry(raw *RawTlogEntry) *jsval.Obj {
	return jsval.NewObj().
		Set("logIndex", raw.LogIndex).
		Set("logId", jsval.NewObj().Set("keyId", base64.StdEncoding.EncodeToString(jsval.Hex(raw.LogIDHex)))).
		Set("kindVersion", jsval.NewObj().Set("kind", raw.Kind).Set("version", raw.Version)).
		Set("integratedTime", raw.IntegratedTime).
		Set("inclusionPromise", jsval.NewObj().Set("signedEntryTimestamp", raw.SignedEntryTimestamp)).
		Set("canonicalizedBody", raw.CanonicalizedBody)
}

// verifyAttestation is Tower's: the statement must name this image, then the DSSE envelope, the
// certificate and the log entry are rebuilt into a v0.1 sigstore bundle (a SET is all cosign's
// annotation carries; v0.3 would demand an inclusion proof) and verified. The tlog entry is used
// only when Rekor's key is trusted; without it the check still runs and fails on the missing
// timestamp, which is the honest answer.
func (f *fetcher) verifyAttestation(payload, payloadType, sig, certPem string, statement any, raw *RawTlogEntry) (verdict, error) {
	match, err := subjectMatches(statement, f.digestHex)
	if err != nil {
		return verdict{}, err
	}
	if !match {
		return verdict{err: ptr("attestation subject digest does not match this image")}, nil
	}
	canVerifyTlog := raw != nil && len(f.tm.tlogs) > 0
	entries := []any{}
	if canVerifyTlog {
		entries = append(entries, v01TlogEntry(raw))
	}
	serialized := jsval.NewObj().
		Set("mediaType", bundleV01).
		Set("verificationMaterial", jsval.NewObj().
			Set("certificate", jsval.NewObj().Set("rawBytes", pemToBase64Der(certPem))).
			Set("tlogEntries", entries)).
		Set("dsseEnvelope", jsval.NewObj().
			Set("payload", payload).
			Set("payloadType", payloadType).
			Set("signatures", []any{jsval.NewObj().Set("sig", sig).Set("keyid", "")}))
	return f.verifySerialized(serialized, nil, canVerifyTlog), nil
}

func (f *fetcher) verifySerialized(serialized any, artifact []byte, withTlog bool) verdict {
	b, err := bundleFromJSON(serialized)
	if err == nil {
		threshold := 0
		if withTlog {
			threshold = 1
		}
		err = f.tm.verifyBundle(b, artifact, threshold)
	}
	if err != nil {
		return verdict{err: ptr(err.Error())}
	}
	return verdict{verified: true}
}

// newFormat reads OCI 1.1 referrers: GHCR's native endpoint first (cosign's order), then the
// referrers tag <repo>:sha256-<digest>, an index of referrer manifests.
func (f *fetcher) newFormat(ctx context.Context) ([]attestation, error) {
	status, body, err := f.reg.getBody(ctx, fmt.Sprintf("/v2/%s/referrers/%s", f.repo, f.digest), f.token, ociIndex)
	if err != nil {
		return nil, err
	}
	if !ok(status) {
		status, body, err = f.reg.getBody(ctx, fmt.Sprintf("/v2/%s/manifests/sha256-%s", f.repo, f.digestHex), f.token, ociIndex)
		if err != nil {
			return nil, err
		}
		if status == http.StatusNotFound {
			return []attestation{}, nil
		}
		if !ok(status) {
			return nil, fmt.Errorf("referrers fallback-tag request failed with %d", status)
		}
	}
	index, err := jsval.Parse(body)
	if err != nil {
		return nil, fmt.Errorf("referrers index: %w", err)
	}
	out := []attestation{}
	for _, ref := range arr(jsval.Get(index, "manifests")) {
		at, isString := jsval.Get(ref, "artifactType").(string)
		if !isString || !strings.HasPrefix(at, sigstoreBundleArtifactPrefix) {
			continue
		}
		status, body, err := f.reg.getBody(ctx, fmt.Sprintf("/v2/%s/manifests/%s", f.repo, jsval.String(jsval.Get(ref, "digest"))), f.token, ociManifest)
		if err != nil {
			return nil, err
		}
		if !ok(status) {
			continue
		}
		manifest, err := jsval.Parse(body)
		if err != nil {
			return nil, fmt.Errorf("referrer manifest: %w", err)
		}
		layerDigest := jsval.Get(jsval.Index(jsval.Get(manifest, "layers"), 0), "digest")
		if !jsval.Truthy(layerDigest) {
			continue
		}
		status, body, err = f.reg.getBody(ctx, fmt.Sprintf("/v2/%s/blobs/%s", f.repo, jsval.String(layerDigest)), f.token, "")
		if err != nil {
			return nil, err
		}
		if !ok(status) {
			continue
		}
		if a, keep := f.newFormatOne(ctx, body); keep {
			out = append(out, a)
		}
	}
	return out, nil
}

func (f *fetcher) newFormatOne(ctx context.Context, blob []byte) (attestation, bool) {
	raw, err := jsval.Parse(blob)
	if err != nil {
		return attestation{}, false
	}
	payload := jsval.Path(raw, "dsseEnvelope", "payload")
	if !jsval.Truthy(payload) {
		return attestation{}, false
	}
	statement, err := jsval.Parse(jsval.Base64(jsval.String(payload)))
	if err != nil {
		return attestation{}, false
	}
	if match, err := subjectMatches(statement, f.digestHex); err != nil || !match {
		return attestation{}, false
	}

	var certPem any = jsval.Undefined
	if rb := jsval.Path(raw, "verificationMaterial", "certificate", "rawBytes"); jsval.Truthy(rb) {
		s, isString := rb.(string)
		if !isString {
			return attestation{}, false // der.match is not a function
		}
		certPem = derBase64ToPem(s)
	}
	tlogEntry := jsval.Index(jsval.Path(raw, "verificationMaterial", "tlogEntries"), 0)
	if jsval.Get(statement, "predicateType") == any(PromotionPredicate) {
		return f.promotionOne(raw, statement, certPem, tlogEntry), true
	}
	// The bundle's own receipt was signed by the Rekor key of the day; Rekor's current copy of the
	// same entry (same body) carries one signed by today's key.
	var fresh *RawTlogEntry
	if li := jsval.Get(tlogEntry, "logIndex"); li != jsval.Undefined && f.rekor != nil {
		fresh = f.rekor.ByIndex(ctx, jsval.String(li))
	}
	body := jsval.Get(tlogEntry, "canonicalizedBody")
	freshMatches := fresh != nil && jsval.Truthy(body) && any(fresh.CanonicalizedBody) == body
	source := SourceNone
	if jsval.Truthy(tlogEntry) {
		source = SourceEmbedded
		if freshMatches {
			source = SourceRekor
		}
	}
	mismatch := fresh != nil && jsval.Truthy(body) && !freshMatches
	var tl *jsval.Obj
	if jsval.Truthy(tlogEntry) {
		tl = jsval.NewObj().Set("logIndex", jsval.Num(jsval.Number(jsval.Get(tlogEntry, "logIndex"))))
		if keyID := jsval.Path(tlogEntry, "logId", "keyId"); jsval.Truthy(keyID) {
			tl.Set("logId", hex.EncodeToString(jsval.Base64(jsval.String(keyID))))
		} else {
			tl.Set("logId", jsval.Undefined)
		}
		if it := jsval.Get(tlogEntry, "integratedTime"); jsval.Truthy(it) {
			tl.Set("integratedTime", jsval.Num(jsval.Number(it)))
		} else {
			tl.Set("integratedTime", jsval.Undefined)
		}
	}

	env := jsval.Get(raw, "dsseEnvelope")
	payloadType := jsval.Get(env, "payloadType")
	sig := jsval.Get(jsval.Index(jsval.Get(env, "signatures"), 0), "sig")
	var v verdict
	switch {
	case f.tmErr != nil:
		v.err = f.tmErr
	case f.tm == nil:
		v.err = ptr("trust root unavailable")
	case !jsval.Truthy(certPem):
		v.err = ptr("attestation is missing its certificate")
	case mismatch:
		v.err = ptr("Rekor's entry at this log index is not the one this attestation claims")
	case freshMatches && jsval.Truthy(payloadType) && jsval.Truthy(sig):
		v, err = f.verifyAttestation(jsval.String(payload), jsval.String(payloadType), jsval.String(sig), certPem.(string), statement, fresh)
		if err != nil {
			return attestation{}, false
		}
	default:
		// The bundle as it came, unless no Rekor key is trusted: then without its tlog entries, so
		// an entry nothing can check doesn't fail what can be checked.
		hasTrustedTlog := len(f.tm.tlogs) > 0 && jsval.Truthy(tlogEntry)
		bundleJSON := raw
		if !hasTrustedTlog {
			vm := jsval.NewObj()
			if o, isObj := jsval.Get(raw, "verificationMaterial").(*jsval.Obj); isObj {
				vm = o.Clone()
			}
			vm.Set("tlogEntries", []any{})
			bundleJSON = spread(raw).Set("verificationMaterial", vm)
		}
		v = f.verifySerialized(bundleJSON, nil, hasTrustedTlog)
	}
	a := finish(spread(statement), certPem, tl, v, source, mismatch)
	return a, true
}

// promotionOne is a promotion attestation: signed on public Sigstore by the promoter workflow
// (promotion.go), so its own trust root and identity, and its log index is public Rekor's: the
// platform Rekor lookup does not apply, and its transparencyLog has no logId.
func (f *fetcher) promotionOne(raw, statement, certPem, tlogEntry any) attestation {
	v := verifyPromotionBundle(raw, f.promotion)
	source := SourceNone
	if jsval.Truthy(tlogEntry) {
		source = SourceEmbedded
	}
	obj := spread(statement)
	obj.Set("certificate", certificateValue(certPem))
	if li := jsval.Get(tlogEntry, "logIndex"); li != jsval.Undefined {
		tl := jsval.NewObj().Set("logIndex", jsval.Num(jsval.Number(li)))
		if it := jsval.Get(tlogEntry, "integratedTime"); jsval.Truthy(it) {
			tl.Set("integratedTime", jsval.Num(jsval.Number(it)))
		} else {
			tl.Set("integratedTime", jsval.Undefined)
		}
		obj.Set("transparencyLog", tl)
	} else {
		obj.Set("transparencyLog", jsval.Undefined)
	}
	obj.Set("verified", v.verified)
	if v.err != nil {
		obj.Set("verificationError", *v.err)
	} else {
		obj.Set("verificationError", jsval.Undefined)
	}
	obj.Set("verificationCode", string(classifyVerification(v.verified, v.err, source, false)))
	obj.Set("logSource", string(source))
	return attestation{obj: obj}
}

// signature reads the classic `cosign sign` tag <repo>:sha256-<digest>.sig: a simple-signing
// payload signed directly (no DSSE), verified as a message signature over the payload's bytes.
// Chains logs it to Rekor without attaching the receipt, so it is looked for next to the
// attestations' entries (hints).
func (f *fetcher) signature(ctx context.Context, hints []float64) ([]attestation, error) {
	status, body, err := f.reg.getBody(ctx, fmt.Sprintf("/v2/%s/manifests/sha256-%s.sig", f.repo, f.digestHex), f.token, ociManifest)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return []attestation{}, nil
	}
	if !ok(status) {
		return nil, fmt.Errorf("signature manifest request failed with %d", status)
	}
	manifest, err := jsval.Parse(body)
	if err != nil {
		return nil, fmt.Errorf("signature manifest: %w", err)
	}
	out := []attestation{}
	for _, layer := range arr(jsval.Get(manifest, "layers")) {
		if a, keep := f.signatureOne(ctx, layer, hints); keep {
			out = append(out, a)
		}
	}
	return out, nil
}

func (f *fetcher) signatureOne(ctx context.Context, layer any, hints []float64) (attestation, bool) {
	layerDigest := jsval.Get(layer, "digest")
	status, payload, err := f.reg.getBody(ctx, fmt.Sprintf("/v2/%s/blobs/%s", f.repo, jsval.String(layerDigest)), f.token, "")
	if err != nil || !ok(status) {
		return attestation{}, false
	}
	ann := jsval.Get(layer, "annotations")
	sigB64 := jsval.Get(ann, "dev.cosignproject.cosign/signature")
	certPem := jsval.Get(ann, "dev.sigstore.cosign/certificate")
	tlog := refreshTlogEntry(ctx, parseCosignTlogBundle(jsval.Get(ann, "dev.sigstore.cosign/bundle")), f.rekor)
	if tlog.entry == nil && f.rekor != nil && jsval.Truthy(sigB64) && len(hints) > 0 {
		d, isString := layerDigest.(string)
		if !isString {
			return attestation{}, false
		}
		if found := findHashedRekord(ctx, f.rekor, strings.Replace(d, "sha256:", "", 1), jsval.String(sigB64), hints); found != nil {
			tlog.entry, tlog.source = found, SourceRekor
		}
	}
	// The simple-signing payload, {critical: {image: {docker-manifest-digest}, identity, type},
	// optional}, kept for display even when it doesn't verify. Not JSON: no predicate.
	predicate, perr := jsval.Parse(payload)
	if perr != nil {
		predicate = jsval.Undefined
	}
	claimed := jsval.Path(predicate, "critical", "image", "docker-manifest-digest")
	claimedStr, claimedIsString := claimed.(string)
	if jsval.Truthy(claimed) && !claimedIsString {
		return attestation{}, false // claimedDigest.replace is not a function
	}

	var v verdict
	switch {
	case f.tmErr != nil:
		v.err = f.tmErr
	case f.tm == nil:
		v.err = ptr("trust root unavailable")
	case !jsval.Truthy(certPem) || !jsval.Truthy(sigB64):
		v.err = ptr("signature is missing its certificate or signature bytes")
	case jsval.Truthy(claimed) && claimedStr != "sha256:"+f.digestHex:
		v.err = ptr("signature payload digest does not match this image")
	case tlog.mismatch:
		v.err = ptr("Rekor's entry at this log index is not the one this signature claims")
	default:
		canVerifyTlog := tlog.entry != nil && len(f.tm.tlogs) > 0
		entries := []any{}
		if canVerifyTlog {
			entries = append(entries, v01TlogEntry(tlog.entry))
		}
		sum := sha256.Sum256(payload)
		serialized := jsval.NewObj().
			Set("mediaType", bundleV01).
			Set("verificationMaterial", jsval.NewObj().
				Set("certificate", jsval.NewObj().Set("rawBytes", pemToBase64Der(jsval.String(certPem)))).
				Set("tlogEntries", entries)).
			Set("messageSignature", jsval.NewObj().
				Set("messageDigest", jsval.NewObj().Set("algorithm", "SHA2_256").Set("digest", base64.StdEncoding.EncodeToString(sum[:]))).
				Set("signature", jsval.String(sigB64)))
		v = f.verifySerialized(serialized, payload, canVerifyTlog)
	}

	obj := jsval.NewObj().
		Set("predicateType", "cosign.sigstore.dev/signature/simple-signing").
		Set("predicate", predicate)
	if jsval.Truthy(claimed) {
		obj.Set("subject", []any{jsval.NewObj().Set("name", f.repo).
			Set("digest", jsval.NewObj().Set("sha256", strings.Replace(claimedStr, "sha256:", "", 1)))})
	} else {
		obj.Set("subject", jsval.Undefined)
	}
	return finish(obj, certPem, summarizeTlogEntry(tlog.entry), v, tlog.source, tlog.mismatch), true
}
