package initjob

import (
	"context"

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

// Delete removes the Secret; one already gone is no error (a run again).
func (k KubeSecret) Delete(ctx context.Context) error {
	err := k.Client.CoreV1().Secrets(k.Namespace).Delete(ctx, k.Name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
