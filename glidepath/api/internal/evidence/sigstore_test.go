package evidence

import (
	"testing"

	"github.com/glidepath/api/internal/jsval"
)

// bundleFromJSON refuses what @sigstore/bundle refuses, in its words.
func TestBundleValidation(t *testing.T) {
	parse := func(s string) any {
		v, err := jsval.Parse([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	env := `"dsseEnvelope":{"payload":"eA==","payloadType":"t","signatures":[{"sig":"eA=="}]}`
	cert := `"certificate":{"rawBytes":"eA=="}`
	entry := `{"logIndex":"1","logId":{"keyId":"eA=="},"kindVersion":{"kind":"intoto","version":"0.0.2"},"integratedTime":"1","canonicalizedBody":"eA=="`
	for _, c := range []struct{ bundle, err string }{
		{`{"mediaType":"application/vnd.dev.sigstore.bundle+json;version=0.1","verificationMaterial":{` + cert + `,"tlogEntries":[]},` + env + `}`, ""},
		{`{"mediaType":"application/vnd.dev.sigstore.bundle+json;version=0.1","verificationMaterial":{` + cert + `,"tlogEntries":[` + entry + `}]},` + env + `}`, "invalid v0.1 bundle"},
		{`{"mediaType":"application/vnd.dev.sigstore.bundle+json;version=0.1","verificationMaterial":{` + cert + `,"tlogEntries":[` + entry + `,"inclusionPromise":{"signedEntryTimestamp":"eA=="}}]},` + env + `}`, ""},
		{`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json","verificationMaterial":{` + cert + `,"tlogEntries":[` + entry + `,"inclusionPromise":{"signedEntryTimestamp":"eA=="}}]},` + env + `}`, "invalid bundle"},
		{`{"mediaType":"application/vnd.dev.sigstore.bundle+json;version=0.2","verificationMaterial":{` + cert + `,"tlogEntries":[` + entry + `}]},` + env + `}`, "invalid v0.2 bundle"},
		{`{"mediaType":"nope","verificationMaterial":{` + cert + `},` + env + `}`, "invalid bundle"},
		{`{"mediaType":"application/vnd.dev.sigstore.bundle+json;version=0.1","verificationMaterial":{},` + env + `}`, "invalid v0.1 bundle"},
		{`{"mediaType":"application/vnd.dev.sigstore.bundle+json;version=0.1","verificationMaterial":{` + cert + `}}`, "invalid v0.1 bundle"},
	} {
		_, err := bundleFromJSON(parse(c.bundle))
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != c.err {
			t.Errorf("%s\n got %q, want %q", c.bundle, got, c.err)
		}
	}
}

func TestNodeName(t *testing.T) {
	// Node's X509Certificate.subject for /C=US/O=Ex, Inc+OU=a=b/CN=x"y<z (checked against Node 26).
	if got := escape2253(`Ex, Inc+OU=a=b`); got != `Ex\, Inc\+OU=a=b` {
		t.Fatal(got)
	}
	if got := escape2253(`x"y<z`); got != `x\"y\<z` {
		t.Fatal(got)
	}
}
