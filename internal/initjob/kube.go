package initjob

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// KubeSecret is the administrator's Secret in the Job's namespace; the Job's Role may delete that name only.
type KubeSecret struct {
	Client    kubernetes.Interface
	Namespace string
	Name      string
}

// ServiceSecrets keeps the services' database passwords: a Secret is made once and never overwritten.
type ServiceSecrets interface {
	// Exists says whether the Secret is there (its login was made by an earlier run).
	Exists(ctx context.Context, ref SecretRef) (bool, error)
	// Create makes the Secret with the one key; an existing one is an error.
	Create(ctx context.Context, ref SecretRef, value string) error
}

// KubeServiceSecrets is the cluster's: the Job's Role in each service's namespace allows get and create.
type KubeServiceSecrets struct {
	Client kubernetes.Interface
}

// Exists reads the Secret's presence, never its data.
func (k KubeServiceSecrets) Exists(ctx context.Context, ref SecretRef) (bool, error) {
	_, err := k.Client.CoreV1().Secrets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// Create makes the Secret, labelled as the Job's.
func (k KubeServiceSecrets) Create(ctx context.Context, ref SecretRef, value string) error {
	_, err := k.Client.CoreV1().Secrets(ref.Namespace).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: ref.Name, Namespace: ref.Namespace,
			Labels: map[string]string{"app.kubernetes.io/managed-by": "hugr-init", "app.kubernetes.io/part-of": "hugr"}},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{ref.Key: []byte(value)},
	}, metav1.CreateOptions{})
	return err
}

// Delete removes the Secret; one already gone is no error (a run again).
func (k KubeSecret) Delete(ctx context.Context) error {
	err := k.Client.CoreV1().Secrets(k.Namespace).Delete(ctx, k.Name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// KubeRecovery keeps OpenBao's recovery key in a Secret of OpenBao's namespace. The Job's Role there may get and update
// that name only, and create (which cannot be narrowed to a name).
type KubeRecovery struct {
	Client    kubernetes.Interface
	Namespace string
	Name      string
}

// Present says whether the Secret is there; its data is not read.
func (k KubeRecovery) Present(ctx context.Context) (bool, error) {
	_, err := k.Client.CoreV1().Secrets(k.Namespace).Get(ctx, k.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// Put keeps the key: a new Secret, or the one an OpenBao made before (its database dropped) replaced.
func (k KubeRecovery) Put(ctx context.Context, key string) error {
	secrets := k.Client.CoreV1().Secrets(k.Namespace)
	current, err := secrets.Get(ctx, k.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = secrets.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: k.Name, Namespace: k.Namespace,
				Labels: map[string]string{"app.kubernetes.io/managed-by": "hugr-init", "app.kubernetes.io/part-of": "hugr"}},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{"key": []byte(key)},
		}, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	current.Data = map[string][]byte{"key": []byte(key)}
	_, err = secrets.Update(ctx, current, metav1.UpdateOptions{})
	return err
}
