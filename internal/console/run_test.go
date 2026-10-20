package console

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/hugr-lab/hugr-platform/api/v1alpha1"
)

// pemPair is a self-signed certificate and its key, PEM.
func pemPair(t *testing.T, name string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		DNSNames: []string{name}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// The console waits for a certificate it can serve with - a Secret made later, a key that does not match - and stops
// nothing else meanwhile.
func TestDoorWaitsForItsCertificate(t *testing.T) {
	dir := t.TempDir()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()
	door := &Door{Addr: addr, CertDir: dir, CertWait: 20 * time.Millisecond, Server: &Server{Log: logr.Discard()}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- door.Start(ctx) }()

	// a certificate whose key is another's: still waiting, not stopped
	cert, _ := pemPair(t, "console")
	_, otherKey := pemPair(t, "other")
	must(t, os.WriteFile(filepath.Join(dir, "tls.crt"), cert, 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "tls.key"), otherKey, 0o600))
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("a bad certificate stopped the door (and with it the operator): %v", err)
	default:
	}
	cert, key := pemPair(t, "console")
	must(t, os.WriteFile(filepath.Join(dir, "tls.crt"), cert, 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "tls.key"), key, 0o600))
	served := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // the test's own
		if err == nil {
			_ = conn.Close()
			served = true
			break
		}
	}
	if !served {
		t.Fatal("the door does not serve once its certificate is good")
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("the door's end: %v", err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type countingReader struct {
	client.Reader
	gets int
}

func (r *countingReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	r.gets++
	return r.Reader.Get(ctx, key, obj, opts...)
}

// A CA bundle edited without a new certificate keeps the pool the channels are built on; a failed read after a good
// one keeps the good one; a failed first read is not repeated by every request.
func TestRootsKeepWhatServes(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	certPEM, _ := pemPair(t, "ca")
	platform := &v1alpha1.HugrPlatform{ObjectMeta: metav1.ObjectMeta{Name: "hugr", Namespace: "hugr"},
		Spec: v1alpha1.HugrPlatformSpec{CABundle: &v1alpha1.CABundleSpec{ConfigMapRef: v1alpha1.ConfigMapKeyRef{
			Name: "hugr-ca", Key: "ca.crt"}}}}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "hugr-ca", Namespace: "hugr"},
		Data: map[string]string{"ca.crt": string(certPEM)}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()
	reader := &countingReader{Reader: c}
	roots := &Roots{Platform: func(context.Context) (*v1alpha1.HugrPlatform, error) { return platform, nil },
		Direct: reader}
	ctx := context.Background()
	first, err := roots.Pool(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cm.Annotations = map[string]string{"edited": "yes"} // a new resource version, the same certificates
	must(t, c.Update(ctx, cm))
	roots.read = time.Now().Add(-rootsFresh)
	if again, err := roots.Pool(ctx); err != nil || again != first {
		t.Errorf("an edit that leaves the certificates keeps the pool: %v", err)
	}
	must(t, c.Delete(ctx, cm))
	roots.read = time.Now().Add(-rootsFresh)
	if again, err := roots.Pool(ctx); err != nil || again != first {
		t.Errorf("a failed read after a good one keeps the good bundle: %v", err)
	}

	missing := &Roots{Platform: roots.Platform, Direct: reader}
	reader.gets = 0
	for range 5 {
		if _, err := missing.Pool(ctx); err == nil {
			t.Fatal("no bundle, no pool")
		}
	}
	if reader.gets != 1 {
		t.Errorf("a failed first read is answered from memory for a while: %d reads", reader.gets)
	}
}
