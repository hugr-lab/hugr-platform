package trustbundle

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// certificate is a self-signed CA certificate, PEM, named cn.
func certificate(t *testing.T, cn string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func names(t *testing.T, bundle string) []string {
	t.Helper()
	var out []string
	rest := []byte(bundle)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return out
		}
		parsed, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatalf("a %s block that is no certificate: %v", block.Type, err)
		}
		out = append(out, parsed.Subject.CommonName)
	}
}

func writer(t *testing.T, objs ...client.Object) (*Writer, client.Client) {
	t.Helper()
	dir := t.TempDir()
	roots, internal := certificate(t, "public root"), certificate(t, "internal CA")
	// the roots file a distroless image carries, with a comment line; the CA file beside the serving certificate
	if err := os.WriteFile(filepath.Join(dir, "roots.crt"), []byte("# public roots\n"+roots), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), []byte(internal), 0o600); err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &Writer{Reader: c, Writer: c, Namespace: "hugr", Name: "hugr-ca-bundle", Key: "ca.crt",
		RootsFile: filepath.Join(dir, "roots.crt"), InternalCAFile: filepath.Join(dir, "ca.crt"), Log: logr.Discard()}, c
}

func read(t *testing.T, c client.Client) corev1.ConfigMap {
	t.Helper()
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "hugr", Name: "hugr-ca-bundle"}, &cm); err != nil {
		t.Fatal(err)
	}
	return cm
}

// TestBundle: the roots, the internal CA and an organisation's CA, each once and only as certificates - a key in a
// source never reaches the bundle; written once, rewritten only when it changes.
func TestBundle(t *testing.T) {
	corp := certificate(t, "corp CA")
	stray := "-----BEGIN PRIVATE KEY-----\nMC4CAQAwBQYDK2VwBCIEIA==\n-----END PRIVATE KEY-----\n"
	source := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "hugr", Name: "corp-ca"},
		Data: map[string]string{"ca.crt": corp + stray + corp}}
	w, c := writer(t, source)
	w.Extra = []Source{{Name: "corp-ca", Key: "ca.crt"}}
	ctx := context.Background()
	if err := w.WriteOnce(ctx); err != nil {
		t.Fatal(err)
	}
	cm := read(t, c)
	if got := strings.Join(names(t, cm.Data["ca.crt"]), ", "); got != "public root, internal CA, corp CA" {
		t.Errorf("the bundle holds %s", got)
	}
	if strings.Contains(cm.Data["ca.crt"], "PRIVATE KEY") || strings.Contains(cm.Data["ca.crt"], "#") {
		t.Error("the bundle carries something that is no certificate")
	}
	if cm.Labels["app.kubernetes.io/managed-by"] != ManagedBy {
		t.Errorf("labels %v", cm.Labels)
	}
	version := cm.ResourceVersion
	if err := w.WriteOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if read(t, c).ResourceVersion != version {
		t.Error("an unchanged bundle was written again")
	}
	// the organisation's CA changes: the bundle follows
	source.Data["ca.crt"] = certificate(t, "corp CA 2")
	if err := c.Update(ctx, source); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(t, read(t, c).Data["ca.crt"]), ", "); got != "public root, internal CA, corp CA 2" {
		t.Errorf("after the change the bundle holds %s", got)
	}
}

// TestBundleRefusals: no roots, a missing source, someone else's ConfigMap - nothing is written; a serving certificate
// brought without its CA leaves the internal CA out.
func TestBundleRefusals(t *testing.T) {
	ctx := context.Background()
	w, _ := writer(t)
	w.RootsFile = filepath.Join(t.TempDir(), "none")
	if err := w.WriteOnce(ctx); err == nil || !strings.Contains(err.Error(), "public roots") {
		t.Errorf("no roots: %v", err)
	}

	w, _ = writer(t)
	w.Extra = []Source{{Name: "corp-ca", Key: "ca.crt"}}
	if err := w.WriteOnce(ctx); err == nil || !strings.Contains(err.Error(), "corp-ca") {
		t.Errorf("a missing source: %v", err)
	}

	theirs := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "hugr", Name: "hugr-ca-bundle"},
		Data: map[string]string{"ca.crt": "theirs"}}
	w, c := writer(t, theirs)
	if err := w.WriteOnce(ctx); err == nil || !strings.Contains(err.Error(), "not the operator's") {
		t.Errorf("someone else's ConfigMap: %v", err)
	}
	if read(t, c).Data["ca.crt"] != "theirs" {
		t.Error("someone else's ConfigMap was written over")
	}

	w, c = writer(t)
	w.InternalCAFile = filepath.Join(t.TempDir(), "ca.crt")
	if err := w.WriteOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(t, read(t, c).Data["ca.crt"]), ", "); got != "public root" {
		t.Errorf("without the internal CA's file the bundle holds %s", got)
	}
}

func TestParseSources(t *testing.T) {
	got, err := ParseSources(" corp-ca/ca.crt, idp/root.pem ,")
	if err != nil || len(got) != 2 || got[0] != (Source{"corp-ca", "ca.crt"}) || got[1] != (Source{"idp", "root.pem"}) {
		t.Errorf("%v %v", got, err)
	}
	for _, bad := range []string{"corp-ca", "/ca.crt", "corp-ca/"} {
		if _, err := ParseSources(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}
