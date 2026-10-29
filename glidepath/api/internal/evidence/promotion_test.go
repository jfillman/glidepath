package evidence

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/glidepath/api/internal/jsval"
)

// testdata/promotion-bundle.json is the real promotion of gate-api 2.0.7-92988b1 to Flight
// (2026-10-29), the bundle Tower's promotionEvidence.test.ts uses: signed on public Sigstore by
// riley-roooo/flight-promoter's promote.yaml on main, with an SCT, a Rekor inclusion proof and an
// RFC 3161 timestamp. Everything here is offline.

func promotionBundle(t *testing.T) any {
	t.Helper()
	b, err := os.ReadFile("testdata/promotion-bundle.json")
	if err != nil {
		t.Fatal(err)
	}
	v, err := jsval.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func promoter(t *testing.T, repo string) *PromotionTrust {
	t.Helper()
	p, err := NewPromotionTrust(repo)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPromotionVerifies(t *testing.T) {
	v := verifyPromotionBundle(promotionBundle(t), promoter(t, "riley-roooo/flight-promoter"))
	if !v.verified {
		t.Fatalf("not verified: %s", *v.err)
	}
}

func TestPromotionWrongIdentity(t *testing.T) {
	v := verifyPromotionBundle(promotionBundle(t), promoter(t, "riley-roooo/other-promoter"))
	if v.verified || !strings.HasPrefix(*v.err, `certificate identity error - expected ^https://github\.com/riley-roooo/other-promoter/`) {
		t.Fatalf("%+v", v)
	}
}

func TestPromotionNoPromoter(t *testing.T) {
	if p := promoter(t, ""); p != nil {
		t.Fatal("a promoter without a repo")
	}
	v := verifyPromotionBundle(promotionBundle(t), nil)
	if v.verified || *v.err != "no promoter configured (glidepath.promotion.promoterRepo)" {
		t.Fatalf("%+v", v)
	}
}

// An edited statement no longer matches the signature, the log entry or the timestamp.
func TestPromotionEdited(t *testing.T) {
	raw := promotionBundle(t)
	env := jsval.Get(raw, "dsseEnvelope").(*jsval.Obj)
	payload, _ := base64.StdEncoding.DecodeString(jsval.String(env.Get("payload")))
	edited := strings.Replace(string(payload), `"itsm":"success"`, `"itsm":"skipped"`, 1)
	if edited == string(payload) {
		t.Fatal("fixture has no itsm gate")
	}
	env.Set("payload", base64.StdEncoding.EncodeToString([]byte(edited)))
	v := verifyPromotionBundle(raw, promoter(t, "riley-roooo/flight-promoter"))
	if v.verified {
		t.Fatal("an edited promotion verified")
	}
}

// Only public Sigstore's root verifies it: the platform's trust material refuses the same bundle.
func TestPromotionNotOnThePlatformRoot(t *testing.T) {
	p := promoter(t, "riley-roooo/flight-promoter")
	b, err := bundleFromJSON(promotionBundle(t))
	if err != nil {
		t.Fatal(err)
	}
	platform := &trustMaterial{cas: []certAuthority{{chain: p.tm.cas[0].chain, start: 0, end: endOfTime}}}
	if err := platform.verify(b, nil, verifyOptions{tlogThreshold: 1}); err == nil {
		t.Fatal("verified without public Sigstore's timestamp authority and logs")
	}
}

// The shape Tower gives it: the statement, its certificate, a transparencyLog with no logId (the
// index is public Rekor's), embedded as its source, and no hint for the platform Rekor.
func TestPromotionShape(t *testing.T) {
	raw := promotionBundle(t)
	payload, _ := base64.StdEncoding.DecodeString(jsval.String(jsval.Path(raw, "dsseEnvelope", "payload")))
	statement, _ := jsval.Parse(payload)
	f := &fetcher{promotion: promoter(t, "riley-roooo/flight-promoter")}
	cert := derBase64ToPem(jsval.String(jsval.Path(raw, "verificationMaterial", "certificate", "rawBytes")))
	a := f.promotionOne(raw, statement, cert, jsval.Index(jsval.Path(raw, "verificationMaterial", "tlogEntries"), 0))
	if a.logIndex != nil {
		t.Fatal("a promotion attestation is a platform Rekor hint")
	}
	got, _ := json.Marshal(a.obj)
	var m map[string]any
	_ = json.Unmarshal(got, &m)
	if m["predicateType"] != PromotionPredicate || m["verified"] != true || m["verificationCode"] != "verified" || m["logSource"] != "embedded" {
		t.Fatalf("%s", got)
	}
	if tl := m["transparencyLog"].(map[string]any); len(tl) != 2 || tl["logIndex"] != 3555857046.0 || tl["integratedTime"] != 1793240865.0 {
		t.Fatalf("transparencyLog %v", tl)
	}
	if id := m["certificate"].(map[string]any)["identity"]; id != "https://github.com/riley-roooo/flight-promoter/.github/workflows/promote.yaml@refs/heads/main" {
		t.Fatalf("identity %v", id)
	}
}

// Each of public Sigstore's checks is really made: take one away and the bundle fails on it.
func TestPromotionEveryCheckCounts(t *testing.T) {
	p := promoter(t, "riley-roooo/flight-promoter")
	for _, c := range []struct {
		name, want string
		edit       func(raw any, tm *trustMaterial)
	}{
		{"no CT log", "SCT verification failed", func(_ any, tm *trustMaterial) { tm.ctlogs = nil }},
		{"no timestamp authority", "timestamp could not be verified", func(_ any, tm *trustMaterial) { tm.tsas = nil }},
		{"no Rekor key", "inclusion promise could not be verified", func(_ any, tm *trustMaterial) { tm.tlogs = nil }},
		// Without the inclusion promise the proof is the only inclusion check (the TSA still dates it).
		{"checkpoint signature edited", "invalid checkpoint signature", func(raw any, _ *trustMaterial) {
			entry := jsval.Path(raw, "verificationMaterial", "tlogEntries").([]any)[0].(*jsval.Obj)
			entry.Set("inclusionPromise", jsval.Undefined)
			env := jsval.Path(entry, "inclusionProof", "checkpoint").(*jsval.Obj)
			e := jsval.String(env.Get("envelope"))
			env.Set("envelope", e[:len(e)-6]+"AAAA=\n")
		}},
		{"wrong issuer", "invalid certificate extension - expected issuer=https://accounts.google.com, got issuer=" + githubActionsIssuer, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			raw := promotionBundle(t)
			tm := *p.tm
			trust := &PromotionTrust{tm: &tm, identity: p.identity}
			if c.edit != nil {
				c.edit(raw, &tm)
			}
			b, err := bundleFromJSON(raw)
			if err != nil {
				t.Fatal(err)
			}
			issuer := githubActionsIssuer
			if c.edit == nil {
				issuer = "https://accounts.google.com"
			}
			err = trust.tm.verify(b, nil, verifyOptions{tlogThreshold: 1, ctlogThreshold: 1,
				policy: &identityPolicy{subjectAlternativeName: trust.identity, issuer: issuer}})
			if err == nil || err.Error() != c.want {
				t.Fatalf("got %v, want %q", err, c.want)
			}
		})
	}
}

// The identity is exact: the same workflow on another branch whose name starts with main, or in a
// repository whose name only starts with the promoter's, is refused.
func TestPromotionIdentityIsExact(t *testing.T) {
	p := promoter(t, "riley-roooo/flight-promoter")
	re := regexp.MustCompile(p.identity)
	for san, want := range map[string]bool{
		"https://github.com/riley-roooo/flight-promoter/.github/workflows/promote.yaml@refs/heads/main":      true,
		"https://github.com/riley-roooo/flight-promoter/.github/workflows/promote.yaml@refs/heads/main-evil": false,
		"https://github.com/riley-roooo/flight-promoterX/.github/workflows/promote.yaml@refs/heads/main":     false,
		"https://github.com/riley-roooo/flight-promoter/.github/workflows/promoteXyaml@refs/heads/main":      false,
		"x https://github.com/riley-roooo/flight-promoter/.github/workflows/promote.yaml@refs/heads/main":    false,
	} {
		if re.MatchString(san) != want {
			t.Errorf("%s: want %v", san, want)
		}
	}
}
