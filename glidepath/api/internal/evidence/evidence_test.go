package evidence

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// A whole supply chain in miniature: a Fulcio-style root (Ed25519, like hangar-dev's) issuing a
// short-lived ECDSA leaf with a URI SAN and no subject, a Rekor key, and an image carrying a legacy
// .att attestation, a referrer bundle with an inclusion proof, and a .sig signature whose Rekor
// entry is only findable next to the attestations'.

const (
	ns       = "app-shop-cicd"
	repo     = "o/shop"
	user     = "robot"
	password = "ghp_SECRET_never_shown_0123456789"
	bearer   = "ghcr-bearer-token"
	identity = "https://kubernetes.io/namespaces/tekton-pipelines/serviceaccounts/tekton-chains-controller"
)

var signedAt = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

type pki struct {
	rootPEM  string
	leafPEM  string
	leafDER  []byte
	leafKey  *ecdsa.PrivateKey
	rekorKey *ecdsa.PrivateKey
	rekorPEM string
	logID    []byte
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	rootPub, rootPriv, _ := ed25519.GenerateKey(rand.Reader)
	rootTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-fulcio-root"},
		NotBefore: signedAt.Add(-24 * time.Hour), NotAfter: signedAt.Add(24 * 365 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTmpl, rootTmpl, rootPub, rootPriv)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := x509.ParseCertificate(rootDER)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	u, _ := url.Parse(identity)
	leafTmpl := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: signedAt.Add(-time.Minute), NotAfter: signedAt.Add(10 * time.Minute),
		URIs: []*url.URL{u}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, root, &leafKey.PublicKey, rootPriv)
	if err != nil {
		t.Fatal(err)
	}
	rekorKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&rekorKey.PublicKey)
	id := sha256.Sum256(spki)
	return &pki{
		rootPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER})),
		leafPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})),
		leafDER:  leafDER,
		leafKey:  leafKey,
		rekorKey: rekorKey,
		rekorPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: spki})),
		logID:    id[:],
	}
}

func sign(k *ecdsa.PrivateKey, data []byte) []byte {
	h := sha256.Sum256(data)
	s, _ := ecdsa.SignASN1(rand.Reader, k, h[:])
	return s
}

func b64s(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func pae(typ string, payload []byte) []byte {
	return []byte(fmt.Sprintf("DSSEv1 %d %s %d %s", len(typ), typ, len(payload), payload))
}

// entry is one Rekor log entry: its body, index, time and SET.
type entry struct {
	body     []byte
	index    int
	time     int64
	setKey   *ecdsa.PrivateKey
	logIDHex string
}

func (e entry) set() []byte {
	payload := fmt.Sprintf(`{"body":%q,"integratedTime":%d,"logID":%q,"logIndex":%d}`, b64s(e.body), e.time, e.logIDHex, e.index)
	return sign(e.setKey, []byte(payload))
}

func (e entry) rekorJSON() []byte {
	return mustJSON(map[string]any{"uuid-" + fmt.Sprint(e.index): map[string]any{"body": b64s(e.body), "integratedTime": e.time,
		"logID": e.logIDHex, "logIndex": e.index, "verification": map[string]any{"signedEntryTimestamp": b64s(e.set())}}})
}

// chain is the image and everything on it, with knobs the tests turn.
type chain struct {
	p            *pki
	digestHex    string
	statement    map[string]any
	legacyEntry  entry
	newEntry     entry
	sigEntry     entry
	sigPayload   []byte
	sigBytes     []byte
	legacyEnv    map[string]any
	legacyAnn    map[string]string
	bundle       map[string]any
	sigAnnBundle string // the .sig layer's own dev.sigstore.cosign/bundle, usually absent

	rekorEntries map[int][]byte // what Rekor answers per index; missing = 404
	rekorDown    bool           // entries 500 (the public key still answers)
	noRekorKey   bool
	noRoot       bool
	tokenCalls   int
	mu           sync.Mutex
}

func newChain(t *testing.T) *chain {
	p := newPKI(t)
	c := &chain{p: p, digestHex: strings.Repeat("ab", 32), rekorEntries: map[int][]byte{}}
	logIDHex := hex.EncodeToString(p.logID)
	when := signedAt.Unix()

	// Legacy .att: Chains' SLSA provenance, logged as intoto 0.0.2.
	c.statement = map[string]any{"_type": "https://in-toto.io/Statement/v0.1", "predicateType": "https://slsa.dev/provenance/v0.2",
		"subject":   []any{map[string]any{"name": "ghcr.io/" + repo, "digest": map[string]any{"sha256": c.digestHex}}},
		"predicate": map[string]any{"builder": map[string]any{"id": "chains"}, "nothing": nil}}
	payload := mustJSON(c.statement)
	sig := sign(p.leafKey, pae("application/vnd.in-toto+json", payload))
	ph := sha256.Sum256(payload)
	c.legacyEntry = entry{index: 100, time: when, setKey: p.rekorKey, logIDHex: logIDHex, body: mustJSON(map[string]any{
		"apiVersion": "0.0.2", "kind": "intoto", "spec": map[string]any{"content": map[string]any{
			"envelope":    map[string]any{"payloadType": "application/vnd.in-toto+json", "signatures": []any{map[string]any{"sig": b64s([]byte(b64s(sig)))}}},
			"payloadHash": map[string]any{"algorithm": "sha256", "value": hex.EncodeToString(ph[:])}}}})}
	c.legacyEnv = map[string]any{"payload": b64s(payload), "payloadType": "application/vnd.in-toto+json", "signatures": []any{map[string]any{"sig": b64s(sig), "keyid": ""}}}
	c.legacyAnn = map[string]string{"dev.sigstore.cosign/certificate": p.leafPEM, "dev.sigstore.cosign/bundle": string(mustJSON(map[string]any{
		"SignedEntryTimestamp": b64s(c.legacyEntry.set()),
		"Payload":              map[string]any{"body": b64s(c.legacyEntry.body), "integratedTime": when, "logIndex": 100, "logID": logIDHex}}))}

	// Referrer: cosign 3's SBOM attestation as a v0.3 bundle, logged as dsse 0.0.1, with a proof.
	sbom := map[string]any{"_type": "https://in-toto.io/Statement/v1", "predicateType": "https://cyclonedx.org/bom",
		"subject": []any{map[string]any{"name": "ghcr.io/" + repo, "digest": map[string]any{"sha256": c.digestHex}}}, "predicate": map[string]any{"bomFormat": "CycloneDX"}}
	sbomPayload := mustJSON(sbom)
	sbomSig := sign(p.leafKey, pae("application/vnd.in-toto+json", sbomPayload))
	sh := sha256.Sum256(sbomPayload)
	c.newEntry = entry{index: 101, time: when + 5, setKey: p.rekorKey, logIDHex: logIDHex, body: mustJSON(map[string]any{
		"apiVersion": "0.0.1", "kind": "dsse", "spec": map[string]any{
			"payloadHash": map[string]any{"algorithm": "sha256", "value": hex.EncodeToString(sh[:])},
			"signatures":  []any{map[string]any{"signature": b64s(sbomSig), "verifier": "x"}}}})}
	// A four-leaf tree with this entry at index 1.
	leaf := func(b []byte) []byte { h := sha256.Sum256(append([]byte{0}, b...)); return h[:] }
	node := func(l, r []byte) []byte { h := sha256.Sum256(append(append([]byte{1}, l...), r...)); return h[:] }
	l0, l2, l3 := leaf([]byte("zero")), leaf([]byte("two")), leaf([]byte("three"))
	n23 := node(l2, l3)
	rootHash := node(node(l0, leaf(c.newEntry.body)), n23)
	note := fmt.Sprintf("rekor-server.rekor-system.svc - 1\n4\n%s\n", b64s(rootHash))
	noteSig := append(append([]byte{}, p.logID[:4]...), sign(p.rekorKey, []byte(note))...)
	checkpoint := note + "\n— rekor-server.rekor-system.svc " + b64s(noteSig) + "\n"
	c.bundle = map[string]any{"mediaType": "application/vnd.dev.sigstore.bundle.v0.3+json",
		"verificationMaterial": map[string]any{"certificate": map[string]any{"rawBytes": b64s(p.leafDER)},
			"tlogEntries": []any{map[string]any{"logIndex": "101", "logId": map[string]any{"keyId": b64s(p.logID)},
				"kindVersion": map[string]any{"kind": "dsse", "version": "0.0.1"}, "integratedTime": fmt.Sprint(when + 5),
				"inclusionPromise": map[string]any{"signedEntryTimestamp": b64s(c.newEntry.set())},
				"inclusionProof": map[string]any{"logIndex": "1", "rootHash": b64s(rootHash), "treeSize": "4",
					"hashes": []any{b64s(l0), b64s(n23)}, "checkpoint": map[string]any{"envelope": checkpoint}},
				"canonicalizedBody": b64s(c.newEntry.body)}}},
		"dsseEnvelope": map[string]any{"payload": b64s(sbomPayload), "payloadType": "application/vnd.in-toto+json",
			"signatures": []any{map[string]any{"sig": b64s(sbomSig), "keyid": ""}}}}

	// .sig: simple signing, logged as hashedrekord at 102 with no receipt on the layer.
	c.sigPayload = mustJSON(map[string]any{"critical": map[string]any{"identity": map[string]any{"docker-reference": "ghcr.io/" + repo},
		"image": map[string]any{"docker-manifest-digest": "sha256:" + c.digestHex}, "type": "cosign container image signature"}, "optional": nil})
	c.sigBytes = sign(p.leafKey, c.sigPayload)
	pd := sha256.Sum256(c.sigPayload)
	c.sigEntry = entry{index: 102, time: when + 9, setKey: p.rekorKey, logIDHex: logIDHex, body: mustJSON(map[string]any{
		"apiVersion": "0.0.1", "kind": "hashedrekord", "spec": map[string]any{
			"data":      map[string]any{"hash": map[string]any{"algorithm": "sha256", "value": hex.EncodeToString(pd[:])}},
			"signature": map[string]any{"content": b64s(c.sigBytes)}}})}

	for _, e := range []entry{c.legacyEntry, c.newEntry, c.sigEntry} {
		c.rekorEntries[e.index] = e.rekorJSON()
	}
	return c
}

func (c *chain) ghcr(t *testing.T) *httptest.Server {
	pd := sha256.Sum256(c.sigPayload)
	sigLayer := "sha256:" + hex.EncodeToString(pd[:])
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			c.mu.Lock()
			c.tokenCalls++
			c.mu.Unlock()
			want := "Basic " + b64s([]byte(user+":"+password))
			if r.Header.Get("Authorization") != want || r.URL.Query().Get("scope") != "repository:"+repo+":pull" {
				// A careless registry that echoes what it got: none of it may reach the caller.
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprintf(w, `{"errors":[{"message":"bad credentials %s"}]}`, r.Header.Get("Authorization"))
				return
			}
			fmt.Fprintf(w, `{"token":%q}`, bearer)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+bearer {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		pre := "/v2/" + repo
		switch r.URL.Path {
		case pre + "/manifests/1.0.0-abc":
			w.Header().Set("Docker-Content-Digest", "sha256:"+c.digestHex)
		case pre + "/manifests/sha256-" + c.digestHex + ".att":
			w.Write(mustJSON(map[string]any{"layers": []any{map[string]any{"digest": "sha256:att1", "annotations": c.legacyAnn}}}))
		case pre + "/blobs/sha256:att1":
			w.Write(mustJSON(c.legacyEnv))
		case pre + "/referrers/sha256:" + c.digestHex:
			http.Error(w, `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`, http.StatusNotFound) // as GHCR answers
		case pre + "/manifests/sha256-" + c.digestHex:
			w.Write(mustJSON(map[string]any{"manifests": []any{
				map[string]any{"digest": "sha256:ref1", "artifactType": "application/vnd.dev.sigstore.bundle.v0.3+json"},
				map[string]any{"digest": "sha256:other", "artifactType": "application/spdx+json"}}}))
		case pre + "/manifests/sha256:ref1":
			w.Write(mustJSON(map[string]any{"layers": []any{map[string]any{"digest": "sha256:bundle1"}}}))
		case pre + "/blobs/sha256:bundle1":
			w.Write(mustJSON(c.bundle))
		case pre + "/manifests/sha256-" + c.digestHex + ".sig":
			ann := map[string]string{"dev.cosignproject.cosign/signature": b64s(c.sigBytes), "dev.sigstore.cosign/certificate": c.p.leafPEM}
			if c.sigAnnBundle != "" {
				ann["dev.sigstore.cosign/bundle"] = c.sigAnnBundle
			}
			w.Write(mustJSON(map[string]any{"layers": []any{map[string]any{"digest": sigLayer, "annotations": ann}}}))
		case pre + "/blobs/" + sigLayer:
			w.Write(c.sigPayload)
		default:
			http.NotFound(w, r)
		}
	}))
}

func (c *chain) rekor() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/log/publicKey":
			if c.noRekorKey {
				http.Error(w, "down", http.StatusServiceUnavailable)
				return
			}
			io.WriteString(w, c.p.rekorPEM)
		case "/api/v1/log/entries":
			if c.rekorDown {
				http.Error(w, "down", http.StatusInternalServerError)
				return
			}
			var i int
			fmt.Sscan(r.URL.Query().Get("logIndex"), &i)
			if b, ok := c.rekorEntries[i]; ok {
				w.Write(b)
				return
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
}

func (c *chain) reader(t *testing.T, logs io.Writer) *Reader {
	gh, rk := c.ghcr(t), c.rekor()
	t.Cleanup(gh.Close)
	t.Cleanup(rk.Close)
	objs := []runtime.Object{&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: PullSecret, Namespace: ns}, Type: corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{".dockerconfigjson": mustJSON(map[string]any{"auths": map[string]any{"ghcr.io": map[string]any{
			"username": user, "password": password, "auth": b64s([]byte(user + ":" + password))}}})}}}
	if !c.noRoot {
		objs = append(objs, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: FulcioRootConfigMap, Namespace: FulcioRootNamespace},
			Data: map[string]string{"cert.pem": c.p.rootPEM}})
	}
	cs := fake.NewSimpleClientset(objs...)
	return &Reader{CS: cs, Credentials: SecretCredentials(cs), RegistryURL: gh.URL, RekorURL: rk.URL,
		HTTP: gh.Client(), Log: slog.New(slog.NewJSONHandler(logs, nil))}
}

func read(t *testing.T, c *chain, image string) (map[string]any, string) {
	t.Helper()
	var logs bytes.Buffer
	p, err := c.reader(t, &logs).Read(context.Background(), ns, image)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{password, b64s([]byte(user + ":" + password)), bearer} {
		if strings.Contains(string(raw), s) || strings.Contains(logs.String(), s) {
			t.Fatalf("a credential reached the answer or the log")
		}
	}
	var out map[string]any
	json.Unmarshal(raw, &out)
	return out, string(raw)
}

type summary struct{ predicateType, code, source, err string }

func summarize(out map[string]any) []summary {
	var s []summary
	for _, a := range out["attestations"].([]any) {
		m := a.(map[string]any)
		e, _ := m["verificationError"].(string)
		s = append(s, summary{m["predicateType"].(string), m["verificationCode"].(string), m["logSource"].(string), e})
	}
	return s
}

func TestEverythingVerifies(t *testing.T) {
	c := newChain(t)
	out, raw := read(t, c, "ghcr.io/"+repo+":1.0.0-abc")
	if out["digest"] != "sha256:"+c.digestHex {
		t.Fatalf("digest %v", out["digest"])
	}
	want := []summary{
		{"https://slsa.dev/provenance/v0.2", "verified", "rekor", ""},
		{"https://cyclonedx.org/bom", "verified", "rekor", ""},
		{"cosign.sigstore.dev/signature/simple-signing", "verified", "rekor", ""},
	}
	if got := summarize(out); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("\n got %v\nwant %v\n%s", got, want, raw)
	}
	if c.tokenCalls != 1 {
		t.Fatalf("one token exchange per request, got %d", c.tokenCalls)
	}
}

// The answer's shape: the statement's own fields first and kept as signed (null included), then
// Tower's, with absent values absent rather than null.
func TestShape(t *testing.T) {
	c := newChain(t)
	_, raw := read(t, c, "ghcr.io/"+repo+"@sha256:"+c.digestHex)
	var top struct{ Attestations []json.RawMessage }
	json.Unmarshal([]byte(raw), &top)
	keys := func(m json.RawMessage) string {
		dec := json.NewDecoder(bytes.NewReader(m))
		dec.Token()
		var ks []string
		for dec.More() {
			k, _ := dec.Token()
			ks = append(ks, k.(string))
			var skip json.RawMessage
			dec.Decode(&skip)
		}
		return strings.Join(ks, ",")
	}
	// encoding/json sorted the fixture's statement keys when it was marshalled; they stay in that order.
	if got := keys(top.Attestations[0]); got != "_type,predicate,predicateType,subject,certificate,transparencyLog,verified,verificationCode,logSource" {
		t.Fatal(got)
	}
	if got := keys(top.Attestations[2]); got != "predicateType,predicate,subject,certificate,transparencyLog,verified,verificationCode,logSource" {
		t.Fatal(got)
	}
	var a struct {
		Predicate       map[string]any
		Certificate     map[string]any
		TransparencyLog map[string]any
	}
	json.Unmarshal(top.Attestations[0], &a)
	if v, ok := a.Predicate["nothing"]; !ok || v != nil {
		t.Fatal("a null in the statement is kept")
	}
	if _, ok := a.Certificate["subject"]; ok {
		t.Fatal("a Fulcio leaf's empty subject is absent, as Node leaves it undefined")
	}
	if a.Certificate["issuer"] != "CN=test-fulcio-root" || a.Certificate["identity"] != identity ||
		a.Certificate["validFrom"] != "Oct 27 11:59:00 2026 GMT" || a.Certificate["validTo"] != "Oct 27 12:10:00 2026 GMT" {
		t.Fatalf("certificate %v", a.Certificate)
	}
	if a.TransparencyLog["logIndex"] != 100.0 || a.TransparencyLog["integratedTime"] != float64(signedAt.Unix()) || len(a.TransparencyLog["logId"].(string)) != 64 {
		t.Fatalf("transparencyLog %v", a.TransparencyLog)
	}
}

func TestRekorDown(t *testing.T) {
	// Entries unreadable: the embedded receipts are checked as they are (still valid here), the
	// referrer bundle by its inclusion proof and checkpoint, and the signature has no entry at all.
	c := newChain(t)
	c.rekorDown = true
	out, raw := read(t, c, "ghcr.io/"+repo+":1.0.0-abc")
	want := []summary{
		{"https://slsa.dev/provenance/v0.2", "verified", "embedded", ""},
		{"https://cyclonedx.org/bom", "verified", "embedded", ""},
		{"cosign.sigstore.dev/signature/simple-signing", "no-log-entry", "none", "expected 1 timestamps, got 0"},
	}
	if got := summarize(out); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("\n got %v\nwant %v\n%s", got, want, raw)
	}
	var top struct{ Attestations []map[string]any }
	json.Unmarshal([]byte(raw), &top)
	if _, ok := top.Attestations[2]["transparencyLog"]; ok {
		t.Fatal("no entry: transparencyLog absent")
	}
}

func TestNoRekorKey(t *testing.T) {
	c := newChain(t)
	c.noRekorKey = true
	out, _ := read(t, c, "ghcr.io/"+repo+":1.0.0-abc")
	for _, s := range summarize(out) {
		if s.code != "no-log-entry" || s.err != "expected 1 timestamps, got 0" {
			t.Fatalf("without Rekor's key nothing can show the certificate was valid when used: %v", s)
		}
	}
}

func TestStaleReceipt(t *testing.T) {
	// The image's own receipt was signed by a Rekor key since lost, and Rekor can't be asked again.
	c := newChain(t)
	old, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	stale := c.legacyEntry
	stale.setKey = old
	var b map[string]any
	json.Unmarshal([]byte(c.legacyAnn["dev.sigstore.cosign/bundle"]), &b)
	b["SignedEntryTimestamp"] = b64s(stale.set())
	c.legacyAnn["dev.sigstore.cosign/bundle"] = string(mustJSON(b))
	c.rekorDown = true
	out, _ := read(t, c, "ghcr.io/"+repo+":1.0.0-abc")
	if s := summarize(out)[0]; s.code != "stale-receipt" || s.err != "inclusion promise could not be verified" {
		t.Fatal(s)
	}
	// With Rekor reachable, its fresh receipt for the same entry verifies.
	c.rekorDown = false
	out, _ = read(t, c, "ghcr.io/"+repo+":1.0.0-abc")
	if s := summarize(out)[0]; s.code != "verified" || s.source != "rekor" {
		t.Fatal(s)
	}
}

func TestLogEntryMismatch(t *testing.T) {
	c := newChain(t)
	other := c.sigEntry
	other.index = 100
	c.rekorEntries[100] = other.rekorJSON()
	out, _ := read(t, c, "ghcr.io/"+repo+":1.0.0-abc")
	s := summarize(out)[0]
	if s.code != "log-entry-mismatch" || s.source != "embedded" || s.err != "Rekor's entry at this log index is not the one this attestation claims" {
		t.Fatal(s)
	}
}

func TestTamperedSignature(t *testing.T) {
	c := newChain(t)
	env := c.legacyEnv
	payload, _ := base64.StdEncoding.DecodeString(env["payload"].(string))
	payload = bytes.Replace(payload, []byte("chains"), []byte("evil!!"), 1)
	env["payload"] = b64s(payload)
	out, _ := read(t, c, "ghcr.io/"+repo+":1.0.0-abc")
	if s := summarize(out)[0]; s.code != "signature-invalid" || s.err != "DSSE payload hash mismatch" {
		t.Fatal(s)
	}
}

func TestSubjectAndDigestMismatch(t *testing.T) {
	c := newChain(t)
	c.digestHex = strings.Repeat("cd", 32) // the image is another one; its attestations name ab...
	out, _ := read(t, c, "ghcr.io/"+repo+":1.0.0-abc")
	got := summarize(out)
	// Legacy: refused by its subject. Referrer: skipped (not this image's). Signature: payload names another digest.
	want := []summary{
		{"https://slsa.dev/provenance/v0.2", "digest-mismatch", "rekor", "attestation subject digest does not match this image"},
		{"cosign.sigstore.dev/signature/simple-signing", "digest-mismatch", "rekor", "signature payload digest does not match this image"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("\n got %v\nwant %v", got, want)
	}
}

func TestMissingCertificate(t *testing.T) {
	c := newChain(t)
	delete(c.legacyAnn, "dev.sigstore.cosign/certificate")
	out, raw := read(t, c, "ghcr.io/"+repo+":1.0.0-abc")
	if s := summarize(out)[0]; s.code != "incomplete" || s.err != "attestation is missing its certificate or signature" {
		t.Fatal(s)
	}
	var top struct{ Attestations []map[string]any }
	json.Unmarshal([]byte(raw), &top)
	if _, ok := top.Attestations[0]["certificate"]; ok {
		t.Fatal("no certificate: absent")
	}
}

func TestNoTrustRoot(t *testing.T) {
	c := newChain(t)
	c.noRoot = true
	out, _ := read(t, c, "ghcr.io/"+repo+":1.0.0-abc")
	for _, s := range summarize(out) {
		if s.code != "trust-root-unavailable" || !strings.HasPrefix(s.err, "could not load Fulcio trust root: ") {
			t.Fatal(s)
		}
	}
}

func TestWrongCA(t *testing.T) {
	c := newChain(t)
	c.p.rootPEM = newPKI(t).rootPEM // the cluster's root rotated since signing
	out, _ := read(t, c, "ghcr.io/"+repo+":1.0.0-abc")
	for _, s := range summarize(out) {
		if s.err != "Failed to verify certificate chain" || s.code != "signature-invalid" {
			t.Fatal(s)
		}
	}
}

func TestImageRefs(t *testing.T) {
	for in, want := range map[string][2]string{
		"ghcr.io/o/a:1.0":                     {"o/a", "1.0"},
		"ghcr.io/o/a@sha256:ff":               {"o/a", "sha256:ff"},
		"ghcr.io/o/a:2.0.4-e9a1266@sha256:ff": {"o/a", "sha256:ff"},
		"ghcr.io/o/team/a:1@sha256:ff":        {"o/team/a", "sha256:ff"},
	} {
		r, ref, err := ParseImageRef(in)
		if err != nil || r != want[0] || ref != want[1] {
			t.Errorf("%s: %s %s %v", in, r, ref, err)
		}
	}
	for _, bad := range []string{"docker.io/o/a:1", "ghcr.io/o/a", "quay.io/ghcr.io/o/a:1"} {
		if _, _, err := ParseImageRef(bad); err == nil {
			t.Errorf("%s accepted", bad)
		} else if _, ok := err.(Invalid); !ok {
			t.Errorf("%s: %T", bad, err)
		}
	}
}

// The registry credential reaches only GHCR's token endpoint: never the answer, an error or a log line.
func TestCredentialNeverEscapes(t *testing.T) {
	c := newChain(t)
	var logs bytes.Buffer
	r := c.reader(t, &logs)
	// A Secret GHCR refuses: its answer echoes the credential back.
	r.Credentials = func(ctx context.Context, namespace string) (basicAuth, error) {
		a, err := SecretCredentials(r.CS)(ctx, namespace)
		return a + "x", err
	}
	_, err := r.Read(context.Background(), ns, "ghcr.io/"+repo+":1.0.0-abc")
	if err == nil || err.Error() != "GHCR token request failed with 401" {
		t.Fatalf("%v", err)
	}
	for _, s := range []string{password, b64s([]byte(user + ":" + password))} {
		if strings.Contains(err.Error(), s) || strings.Contains(logs.String(), s) {
			t.Fatal("the credential leaked")
		}
	}
	// A missing Secret is an anonymous exchange (Tower's fallback), which this registry refuses.
	r = c.reader(t, &logs)
	r.CS.CoreV1().Secrets(ns).Delete(context.Background(), PullSecret, metav1.DeleteOptions{})
	if _, err := r.Read(context.Background(), ns, "ghcr.io/"+repo+":1.0.0-abc"); err == nil || err.Error() != "GHCR token request failed with 401" {
		t.Fatalf("%v", err)
	}
}

func TestInclusionProof(t *testing.T) {
	// Rekor unreachable, so the referrer bundle stands on its own proof and checkpoint.
	proof := func(c *chain) map[string]any {
		return c.bundle["verificationMaterial"].(map[string]any)["tlogEntries"].([]any)[0].(map[string]any)["inclusionProof"].(map[string]any)
	}
	c := newChain(t)
	c.rekorDown = true
	proof(c)["hashes"].([]any)[0] = b64s(make([]byte, 32))
	out, _ := read(t, c, "ghcr.io/"+repo+":1.0.0-abc")
	if s := summarize(out)[1]; s.err != "calculated root hash does not match inclusion proof" || s.code != "signature-invalid" {
		t.Fatal(s)
	}
	c = newChain(t)
	c.rekorDown = true
	cp := proof(c)["checkpoint"].(map[string]any)
	cp["envelope"] = strings.Replace(cp["envelope"].(string), "\n4\n", "\n5\n", 1)
	out, _ = read(t, c, "ghcr.io/"+repo+":1.0.0-abc")
	if s := summarize(out)[1]; s.err != "invalid checkpoint signature" || s.code != "stale-receipt" {
		t.Fatal(s)
	}
	c = newChain(t)
	c.rekorDown = true
	delete(proof(c), "checkpoint")
	out, _ = read(t, c, "ghcr.io/"+repo+":1.0.0-abc")
	if s := summarize(out)[1]; s.err != "invalid bundle" {
		t.Fatal("a v0.3 bundle needs its checkpoint:", s)
	}
}

// setLegacy logs the legacy attestation as e: Rekor's copy and the image's own receipt both.
func (c *chain) setLegacy(e entry) {
	c.legacyEntry = e
	c.rekorEntries[e.index] = e.rekorJSON()
	c.legacyAnn["dev.sigstore.cosign/bundle"] = string(mustJSON(map[string]any{"SignedEntryTimestamp": b64s(e.set()),
		"Payload": map[string]any{"body": b64s(e.body), "integratedTime": e.time, "logIndex": e.index, "logID": e.logIDHex}}))
}

func TestLogBodyNamesAnotherSignature(t *testing.T) {
	c := newChain(t)
	e := c.legacyEntry
	var body map[string]any
	json.Unmarshal(e.body, &body)
	env := body["spec"].(map[string]any)["content"].(map[string]any)["envelope"].(map[string]any)
	env["signatures"] = []any{map[string]any{"sig": b64s([]byte(b64s([]byte("another signature"))))}}
	e.body = mustJSON(body)
	c.setLegacy(e)
	out, _ := read(t, c, "ghcr.io/"+repo+":1.0.0-abc")
	if s := summarize(out)[0]; s.err != "tlog entry signature mismatch" || s.code != "signature-invalid" || s.source != "rekor" {
		t.Fatal(s)
	}
}

func TestCertificateExpiredWhenLogged(t *testing.T) {
	// Logged an hour after the ten-minute certificate expired: nothing shows it was valid when used.
	c := newChain(t)
	e := c.legacyEntry
	e.time += 3600
	c.setLegacy(e)
	out, _ := read(t, c, "ghcr.io/"+repo+":1.0.0-abc")
	if s := summarize(out)[0]; s.err != "Failed to verify certificate chain" {
		t.Fatal(s)
	}
}

func TestSCTWithoutCTLog(t *testing.T) {
	// No CT log is trusted, so a certificate carrying an SCT can't verify (ctlogThreshold 0 or not).
	rootPub, rootPriv, _ := ed25519.GenerateKey(rand.Reader)
	rt := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "r"}, NotBefore: signedAt.Add(-time.Hour),
		NotAfter: signedAt.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	rootDER, _ := x509.CreateCertificate(rand.Reader, rt, rt, rootPub, rootPriv)
	root, _ := x509.ParseCertificate(rootDER)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	list, _ := asn1.Marshal([]byte{0, 6, 0, 4, 1, 2, 3, 4}) // one 4-byte SCT
	for _, withSCT := range []bool{false, true} {
		lt := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: signedAt.Add(-time.Minute), NotAfter: signedAt.Add(time.Minute)}
		if withSCT {
			lt.ExtraExtensions = []pkix.Extension{{Id: oidSCT, Value: list}}
		}
		der, _ := x509.CreateCertificate(rand.Reader, lt, root, &leafKey.PublicKey, rootPriv)
		leaf, _ := x509.ParseCertificate(der)
		err := (&trustMaterial{root: root}).verifyCertificate(leaf, []float64{float64(signedAt.UnixMilli())})
		if withSCT && (err == nil || err.Error() != "SCT verification failed") || !withSCT && err != nil {
			t.Fatalf("sct=%v: %v", withSCT, err)
		}
	}
}

func TestSignatureNotByTheCertificate(t *testing.T) {
	// The log records exactly what the envelope carries, and both are a signature the leaf never made.
	c := newChain(t)
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	payload, _ := base64.StdEncoding.DecodeString(c.legacyEnv["payload"].(string))
	forged := sign(other, pae("application/vnd.in-toto+json", payload))
	c.legacyEnv["signatures"] = []any{map[string]any{"sig": b64s(forged), "keyid": ""}}
	e := c.legacyEntry
	var body map[string]any
	json.Unmarshal(e.body, &body)
	body["spec"].(map[string]any)["content"].(map[string]any)["envelope"].(map[string]any)["signatures"] = []any{map[string]any{"sig": b64s([]byte(b64s(forged)))}}
	e.body = mustJSON(body)
	c.setLegacy(e)
	out, _ := read(t, c, "ghcr.io/"+repo+":1.0.0-abc")
	if s := summarize(out)[0]; s.err != "signature verification failed" || s.code != "signature-invalid" {
		t.Fatal(s)
	}
}

func TestSignatureReceiptForAnotherSignature(t *testing.T) {
	// A .sig layer whose own receipt is a hashedrekord entry for a different signature.
	c := newChain(t)
	e := c.sigEntry
	var body map[string]any
	json.Unmarshal(e.body, &body)
	body["spec"].(map[string]any)["signature"] = map[string]any{"content": b64s([]byte("not this one"))}
	e.body = mustJSON(body)
	c.rekorEntries[e.index] = e.rekorJSON()
	ann := string(mustJSON(map[string]any{"SignedEntryTimestamp": b64s(e.set()),
		"Payload": map[string]any{"body": b64s(e.body), "integratedTime": e.time, "logIndex": e.index, "logID": e.logIDHex}}))
	c.sigAnnBundle = ann
	out, _ := read(t, c, "ghcr.io/"+repo+":1.0.0-abc")
	if s := summarize(out)[2]; s.err != "signature mismatch" || s.code != "signature-invalid" || s.source != "rekor" {
		t.Fatal(s)
	}
}
