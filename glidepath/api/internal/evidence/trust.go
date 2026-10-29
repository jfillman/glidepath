package evidence

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// FulcioRootNamespace/FulcioRootConfigMap hold the cluster's Fulcio root (cert.pem), the same
	// ConfigMap verify-image-provenance.yaml reads. Every ServiceAccount may get it
	// (templates/sigstore/fulcio-root-configmap.yaml).
	FulcioRootNamespace = "platform-catalog"
	FulcioRootConfigMap = "fulcio-root-ca"
	// trustTTL is how long the root and Rekor's key are kept, as Tower keeps them.
	trustTTL = 10 * time.Minute
)

// trustSource fetches and caches the trust material: the Fulcio root from its ConfigMap, Rekor's
// public key from Rekor itself (there is no ConfigMap for it; it is fetched live, as every
// consumer does). Failures are not cached.
type trustSource struct {
	cs    kubernetes.Interface
	rekor func(ctx context.Context, path string) ([]byte, error)

	mu       sync.Mutex
	root     string
	rootAt   time.Time
	rekorKey string
	rekorAt  time.Time
}

func (t *trustSource) fulcioRoot(ctx context.Context) (string, error) {
	t.mu.Lock()
	if t.root != "" && time.Since(t.rootAt) < trustTTL {
		defer t.mu.Unlock()
		return t.root, nil
	}
	t.mu.Unlock()
	cm, err := t.cs.CoreV1().ConfigMaps(FulcioRootNamespace).Get(ctx, FulcioRootConfigMap, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	p := cm.Data["cert.pem"]
	if p == "" {
		return "", errors.New("fulcio-root-ca ConfigMap has no cert.pem key")
	}
	t.mu.Lock()
	t.root, t.rootAt = p, time.Now()
	t.mu.Unlock()
	return p, nil
}

func (t *trustSource) rekorPublicKey(ctx context.Context) (string, error) {
	t.mu.Lock()
	if t.rekorKey != "" && time.Since(t.rekorAt) < trustTTL {
		defer t.mu.Unlock()
		return t.rekorKey, nil
	}
	t.mu.Unlock()
	body, err := t.rekor(ctx, "/api/v1/log/publicKey")
	if err != nil {
		return "", err
	}
	t.mu.Lock()
	t.rekorKey, t.rekorAt = string(body), time.Now()
	t.mu.Unlock()
	return string(body), nil
}

// buildTrustMaterial is Tower's: the root as the one CA, and Rekor's key (when there is one) as the
// one log, its logID the SHA-256 of the key's DER, computed here rather than trusted from any
// bundle.
func buildTrustMaterial(rootPem, rekorPem string) (*trustMaterial, error) {
	block, _ := pem.Decode([]byte(rootPem))
	if block == nil {
		return nil, errors.New("fulcio root is not PEM")
	}
	root, err := parseCert(block.Bytes)
	if err != nil {
		return nil, err
	}
	tm := &trustMaterial{cas: []certAuthority{{chain: []*x509.Certificate{root}, start: ms(root.NotBefore), end: ms(root.NotAfter)}}}
	if rekorPem != "" {
		kb, _ := pem.Decode([]byte(rekorPem))
		if kb == nil {
			return nil, errors.New("rekor public key is not PEM")
		}
		key, err := x509.ParsePKIXPublicKey(kb.Bytes)
		if err != nil {
			return nil, fmt.Errorf("rekor public key: %w", err)
		}
		der, err := x509.MarshalPKIXPublicKey(key)
		if err != nil {
			return nil, err
		}
		id := sha256.Sum256(der)
		tm.tlogs = append(tm.tlogs, tlogAuthority{logID: id[:], baseURL: tlogBaseURL, key: key, start: 0, end: endOfTime})
	}
	return tm, nil
}
