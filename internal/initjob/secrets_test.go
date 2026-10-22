package initjob

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func secretsConfig() *SecretsConfig {
	return &SecretsConfig{
		TresorKEK: &SecretRef{Namespace: "hugr-tresor", Name: "tresor-kek", Key: "key"},
		KistaKey:  &SecretRef{Namespace: "hugr-kista", Name: "kista-channel-key", Key: "channel"},
		Database: &DatabaseSecrets{Superuser: SecretRef{Namespace: "hugr-database", Name: "postgres-superuser"},
			Admin: SecretRef{Namespace: "hugr", Name: "hugr-init-db-admin"}, Username: "postgres"},
	}
}

func TestRunSecrets(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := fake.NewClientset()
	secrets := KubeSecrets{Client: client}
	if err := RunSecrets(ctx, secretsConfig(), secrets, log); err != nil {
		t.Fatal(err)
	}
	get := func(namespace, name string) map[string][]byte {
		t.Helper()
		s, err := client.CoreV1().Secrets(namespace).Get(ctx, name, metavOpts())
		if err != nil {
			t.Fatalf("%s/%s: %v", namespace, name, err)
		}
		return s.Data
	}
	kek, err := base64.StdEncoding.DecodeString(string(get("hugr-tresor", "tresor-kek")["key"]))
	if err != nil || len(kek) != 32 {
		t.Errorf("tresor's KEK is not 32 bytes in base64: %d, %v", len(kek), err)
	}
	block, _ := pem.Decode(get("hugr-kista", "kista-channel-key")["channel"])
	if block == nil || block.Type != "PRIVATE KEY" {
		t.Fatal("kista's key is no PKCS#8 PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if rsaKey, ok := key.(*rsa.PrivateKey); err != nil || !ok || rsaKey.N.BitLen() != 2048 || rsaKey.E != 65537 {
		t.Errorf("kista's key is not RSA-2048 (e 65537): %v", err)
	}
	super, admin := get("hugr-database", "postgres-superuser"), get("hugr", "hugr-init-db-admin")
	if string(super["username"]) != "postgres" || len(super["password"]) < 32 ||
		string(admin["password"]) != string(super["password"]) {
		t.Error("the superuser and the administrator's copy are not one credential")
	}

	// a run again replaces nothing - not even the administrator's copy, which the catalog phase deleted
	before := string(get("hugr-tresor", "tresor-kek")["key"]) + string(get("hugr-kista", "kista-channel-key")["channel"]) +
		string(super["password"])
	_ = client.CoreV1().Secrets("hugr").Delete(ctx, "hugr-init-db-admin", deleteOpts())
	if err := RunSecrets(ctx, secretsConfig(), secrets, log); err != nil {
		t.Fatal(err)
	}
	after := string(get("hugr-tresor", "tresor-kek")["key"]) + string(get("hugr-kista", "kista-channel-key")["channel"]) +
		string(get("hugr-database", "postgres-superuser")["password"])
	if after != before {
		t.Error("a run again replaced a secret")
	}
	if _, err := client.CoreV1().Secrets("hugr").Get(ctx, "hugr-init-db-admin", metavOpts()); err == nil {
		t.Error("a run again made the administrator's copy, which the catalog phase deleted")
	}
}

func TestLoadSecrets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.yaml")
	write := func(s string) {
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("tresorKEK: {namespace: hugr-tresor, name: tresor-kek, key: key}\n")
	if c, err := LoadSecrets(path); err != nil || c.TresorKEK == nil || c.KistaKey != nil || c.Database != nil {
		t.Errorf("tresor's KEK only: %+v %v", c, err)
	}
	write("kistaKey: {namespace: hugr-kista, name: k}\n")
	if _, err := LoadSecrets(path); err == nil {
		t.Error("a secret without its key is read")
	}
	write("unknown: 1\n")
	if _, err := LoadSecrets(path); err == nil {
		t.Error("an unknown field is read")
	}
}

func metavOpts() metav1.GetOptions     { return metav1.GetOptions{} }
func deleteOpts() metav1.DeleteOptions { return metav1.DeleteOptions{} }
