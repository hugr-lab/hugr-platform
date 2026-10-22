package initjob

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"log/slog"
	"os"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
)

// SecretsConfig is the secrets phase's (spec 005 step 3): the secrets of the umbrella's own parts, each made once in
// its part's namespace. A part the release does not run has none here.
type SecretsConfig struct {
	// TresorKEK: its own tresor's KEK, 32 random bytes in base64 (tresor-server's local key)
	TresorKEK *SecretRef `json:"tresorKEK,omitempty"`
	// KistaKey: its own kista's channel key, RSA-2048 in PKCS#8 PEM (kista's kubesecret source)
	KistaKey *SecretRef `json:"kistaKey,omitempty"`
	// Database: its own catalog database's superuser, and the one-time administrator Secret the catalog phase reads
	Database *DatabaseSecrets `json:"database,omitempty"`
}

// DatabaseSecrets are the own database's: its superuser's Secret (username, password) in the database's namespace,
// and the same credential as the catalog phase's one-time administrator Secret, which that phase deletes.
type DatabaseSecrets struct {
	Superuser SecretRef `json:"superuser"`
	Admin     SecretRef `json:"admin"`
	Username  string    `json:"username"`
}

// LoadSecrets reads the phase's configuration.
func LoadSecrets(path string) (*SecretsConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c SecretsConfig
	if err := yaml.UnmarshalStrict(raw, &c); err != nil {
		return nil, fmt.Errorf("hugr-init: the secrets phase's configuration: %w", err)
	}
	named := func(ref SecretRef) bool { return dnsLabel.MatchString(ref.Namespace) && dnsName.MatchString(ref.Name) }
	for _, ref := range []*SecretRef{c.TresorKEK, c.KistaKey} {
		if ref != nil && (!named(*ref) || !secretKey.MatchString(ref.Key)) {
			return nil, fmt.Errorf("hugr-init: a secret names its namespace, name and key")
		}
	}
	if d := c.Database; d != nil && (!named(d.Superuser) || !named(d.Admin) || !plainName.MatchString(d.Username)) {
		return nil, fmt.Errorf("hugr-init: the database's secrets name their namespaces and names, and the superuser")
	}
	return &c, nil
}

// Secrets is what the phase asks of the cluster: a Secret's presence, a new one, and the one-time administrator's
// replaced.
type Secrets interface {
	Exists(ctx context.Context, namespace, name string) (bool, error)
	Create(ctx context.Context, namespace, name string, data map[string][]byte) error
	// Put creates the Secret, or replaces its data
	Put(ctx context.Context, namespace, name string, data map[string][]byte) error
}

// RunSecrets makes each of the own parts' secrets that is not there yet, and never replaces one: a KEK, a channel key
// or a database password made again would lose what the old one protects.
func RunSecrets(ctx context.Context, c *SecretsConfig, secrets Secrets, log *slog.Logger) error {
	if ref := c.TresorKEK; ref != nil {
		if err := once(ctx, secrets, ref.Namespace, ref.Name, log, func() (map[string][]byte, error) {
			key := make([]byte, 32)
			if _, err := rand.Read(key); err != nil {
				return nil, err
			}
			return map[string][]byte{ref.Key: []byte(base64.StdEncoding.EncodeToString(key))}, nil
		}); err != nil {
			return err
		}
	}
	if ref := c.KistaKey; ref != nil {
		if err := once(ctx, secrets, ref.Namespace, ref.Name, log, func() (map[string][]byte, error) {
			key, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				return nil, err
			}
			der, err := x509.MarshalPKCS8PrivateKey(key)
			if err != nil {
				return nil, err
			}
			return map[string][]byte{ref.Key: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})}, nil
		}); err != nil {
			return err
		}
	}
	if d := c.Database; d != nil {
		exists, err := secrets.Exists(ctx, d.Superuser.Namespace, d.Superuser.Name)
		if err != nil {
			return fmt.Errorf("hugr-init: the Secret %s/%s: %w", d.Superuser.Namespace, d.Superuser.Name, err)
		}
		if exists {
			log.Info("a secret is made already: left as it is", "secret", d.Superuser.Namespace+"/"+d.Superuser.Name)
			return nil
		}
		password, err := newPassword()
		if err != nil {
			return err
		}
		data := map[string][]byte{"username": []byte(d.Username), "password": []byte(password)}
		// the administrator's copy first: a run stopped between the two makes both again, the copy replaced
		if err := secrets.Put(ctx, d.Admin.Namespace, d.Admin.Name, data); err != nil {
			return fmt.Errorf("hugr-init: the Secret %s/%s: %w", d.Admin.Namespace, d.Admin.Name, err)
		}
		if err := secrets.Create(ctx, d.Superuser.Namespace, d.Superuser.Name, data); err != nil {
			return fmt.Errorf("hugr-init: the Secret %s/%s: %w", d.Superuser.Namespace, d.Superuser.Name, err)
		}
		log.Info("a secret is made", "secret", d.Superuser.Namespace+"/"+d.Superuser.Name,
			"administrator", d.Admin.Namespace+"/"+d.Admin.Name)
	}
	return nil
}

func once(ctx context.Context, secrets Secrets, namespace, name string, log *slog.Logger,
	make func() (map[string][]byte, error)) error {
	exists, err := secrets.Exists(ctx, namespace, name)
	if err != nil {
		return fmt.Errorf("hugr-init: the Secret %s/%s: %w", namespace, name, err)
	}
	if exists {
		log.Info("a secret is made already: left as it is", "secret", namespace+"/"+name)
		return nil
	}
	data, err := make()
	if err != nil {
		return err
	}
	if err := secrets.Create(ctx, namespace, name, data); err != nil {
		return fmt.Errorf("hugr-init: the Secret %s/%s: %w", namespace, name, err)
	}
	log.Info("a secret is made", "secret", namespace+"/"+name)
	return nil
}

// KubeSecrets is the cluster's: the Job's Role in each part's namespace allows get and create, and in its own namespace
// get and update of the administrator's Secret by name.
type KubeSecrets struct {
	Client kubernetes.Interface
}

func secretOf(namespace, name string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace,
			Labels: map[string]string{"app.kubernetes.io/managed-by": "hugr-init", "app.kubernetes.io/part-of": "hugr"}},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
}

// Exists reads a Secret's presence, never its data.
func (k KubeSecrets) Exists(ctx context.Context, namespace, name string) (bool, error) {
	_, err := k.Client.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// Create makes a Secret; an existing one is an error.
func (k KubeSecrets) Create(ctx context.Context, namespace, name string, data map[string][]byte) error {
	_, err := k.Client.CoreV1().Secrets(namespace).Create(ctx, secretOf(namespace, name, data), metav1.CreateOptions{})
	return err
}

// Put creates a Secret or replaces its data.
func (k KubeSecrets) Put(ctx context.Context, namespace, name string, data map[string][]byte) error {
	secrets := k.Client.CoreV1().Secrets(namespace)
	current, err := secrets.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = secrets.Create(ctx, secretOf(namespace, name, data), metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	current.Data = data
	_, err = secrets.Update(ctx, current, metav1.UpdateOptions{})
	return err
}
