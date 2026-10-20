// Package trustbundle writes the platform's CA bundle (spec 005): one ConfigMap of the namespace that holds the
// public roots the operator's image carries, the platform's internal CA, and the organisation's extra CAs. A node reads
// it as HUGR_CA_FILE, which replaces its system roots for httpfs, and the front and the console verify the nodes by it.
//
// The internal CA is the issuer of the operator's own serving certificate: the ca.crt cert-manager writes beside it,
// in the directory the operator already mounts. So the operator needs no right to the CA's Secret, which holds the
// CA's key. The CA and its Issuer are the namespace's own, so only a pod or a Certificate of this namespace may be
// issued from it (spec 001's boundary). Only CERTIFICATE blocks enter the bundle: a key or anything else a source
// holds is dropped.
package trustbundle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;create;update

// Source is a ConfigMap of the namespace and the key of its PEM: an organisation's own CA (an IdP's, a database's).
type Source struct {
	Name string
	Key  string
}

// ParseSources reads name/key[,name/key...] (the operator's --ca-bundle-extra).
func ParseSources(text string) ([]Source, error) {
	var out []Source
	for _, item := range strings.Split(text, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, key, ok := strings.Cut(item, "/")
		if !ok || name == "" || key == "" {
			return nil, fmt.Errorf("%q is not name/key", item)
		}
		out = append(out, Source{Name: name, Key: key})
	}
	return out, nil
}

// Writer keeps the bundle's ConfigMap current.
type Writer struct {
	Reader    client.Reader // the API server, directly: no informer for ConfigMaps
	Writer    client.Writer
	Namespace string
	// Name and Key of the ConfigMap the bundle goes into (the HugrPlatform's caBundle names the same).
	Name string
	Key  string
	// RootsFile holds the image's public roots; InternalCAFile the issuer of the operator's serving certificate.
	RootsFile      string
	InternalCAFile string
	Extra          []Source
	// Interval between checks: the internal CA's file follows its Secret, a source's ConfigMap may change.
	Interval time.Duration
	Log      logr.Logger
}

// ManagedBy marks the ConfigMap as the operator's.
const ManagedBy = "hugr-platform"

// NeedLeaderElection: one writer, the leader.
func (w *Writer) NeedLeaderElection() bool { return true }

// Start writes the bundle now, then at every interval until the context ends. A failed write is logged and tried
// again at the next interval; the last bundle written stays.
func (w *Writer) Start(ctx context.Context) error {
	interval := w.Interval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := w.WriteOnce(ctx); err != nil {
			w.Log.Error(err, "the CA bundle", "configmap", w.Name)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Compose reads the sources: the roots (required), the internal CA (when its file is there), the extra ConfigMaps
// (each required). The bundle is their certificates, each once, in that order.
func (w *Writer) Compose(ctx context.Context) ([]byte, error) {
	var parts [][]byte
	roots, err := os.ReadFile(w.RootsFile)
	if err != nil {
		return nil, fmt.Errorf("the public roots %s: %w", w.RootsFile, err)
	}
	parts = append(parts, roots)
	if w.InternalCAFile != "" {
		internal, err := os.ReadFile(w.InternalCAFile)
		switch {
		case err == nil:
			parts = append(parts, internal)
		case errors.Is(err, os.ErrNotExist):
			// a serving certificate brought without its CA (config/default with an administrator's Secret): the bundle
			// is the roots and the extra CAs, and the administrator adds the CA as an extra source
		default:
			return nil, fmt.Errorf("the internal CA %s: %w", w.InternalCAFile, err)
		}
	}
	for _, source := range w.Extra {
		var cm corev1.ConfigMap
		if err := w.Reader.Get(ctx, types.NamespacedName{Namespace: w.Namespace, Name: source.Name}, &cm); err != nil {
			return nil, fmt.Errorf("the CA source %s/%s: %w", source.Name, source.Key, err)
		}
		pemText, ok := cm.Data[source.Key]
		if !ok {
			return nil, fmt.Errorf("the CA source %s has no key %s", source.Name, source.Key)
		}
		parts = append(parts, []byte(pemText))
	}
	return Certificates(parts...)
}

// Certificates joins the CERTIFICATE blocks of the inputs, each once (by its DER), and drops every other block.
func Certificates(inputs ...[]byte) ([]byte, error) {
	var out bytes.Buffer
	seen := map[[sha256.Size]byte]bool{}
	for _, input := range inputs {
		rest := input
		for {
			var block *pem.Block
			block, rest = pem.Decode(rest)
			if block == nil {
				break
			}
			if block.Type != "CERTIFICATE" {
				continue
			}
			sum := sha256.Sum256(block.Bytes)
			if seen[sum] {
				continue
			}
			seen[sum] = true
			if err := pem.Encode(&out, &pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes}); err != nil {
				return nil, err
			}
		}
	}
	if out.Len() == 0 {
		return nil, errors.New("no certificate in any source")
	}
	return out.Bytes(), nil
}

// WriteOnce composes the bundle and writes it when it differs from what the ConfigMap holds.
func (w *Writer) WriteOnce(ctx context.Context) error {
	bundle, err := w.Compose(ctx)
	if err != nil {
		return err
	}
	var cm corev1.ConfigMap
	err = w.Reader.Get(ctx, types.NamespacedName{Namespace: w.Namespace, Name: w.Name}, &cm)
	if apierrors.IsNotFound(err) {
		cm = corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: w.Namespace, Name: w.Name,
				Labels: map[string]string{"app.kubernetes.io/managed-by": ManagedBy, "app.kubernetes.io/part-of": ManagedBy}},
			Data: map[string]string{w.Key: string(bundle)},
		}
		if err := w.Writer.Create(ctx, &cm); err != nil {
			return err
		}
		w.Log.Info("the CA bundle written", "configmap", w.Name)
		return nil
	}
	if err != nil {
		return err
	}
	if cm.Labels["app.kubernetes.io/managed-by"] != ManagedBy {
		// someone else's ConfigMap of that name: never written over
		return fmt.Errorf("the ConfigMap %s is not the operator's (no managed-by %s): the CA bundle is not written",
			w.Name, ManagedBy)
	}
	if cm.Data[w.Key] == string(bundle) {
		return nil
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[w.Key] = string(bundle)
	if err := w.Writer.Update(ctx, &cm); err != nil {
		return err
	}
	w.Log.Info("the CA bundle rewritten", "configmap", w.Name)
	return nil
}
