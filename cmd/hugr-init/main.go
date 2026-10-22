// hugr-init is the platform's install Job (spec 005): the policy catalog's database and login, the login into tresor
// as the catalog's secret, its grants to the groups' identities, then the database administrator's Secret deleted.
// --phase secrets (a Job of the umbrella) makes its own parts' secrets once: its tresor's KEK, its kista's channel key,
// its catalog database's superuser (spec 005 step 3).
// --phase crds (a pre-upgrade hook, spec 008) applies the release's CRDs, which Helm never upgrades from crds/.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/hugr-lab/hugr-platform/internal/crds"
	"github.com/hugr-lab/hugr-platform/internal/initjob"
)

func main() {
	config := flag.String("config", "/etc/hugr-init/config.yaml", "the Job's configuration")
	phase := flag.String("phase", "catalog",
		"catalog (the catalog's secret, its grants), secrets (the umbrella's own parts') or crds (the release's CRDs, "+
			"before an upgrade)")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(*config, *phase, log); err != nil {
		log.Error("hugr-init failed", "error", err.Error())
		os.Exit(1)
	}
	log.Info("hugr-init done")
}

func run(path, phase string, log *slog.Logger) error {
	if phase == "secrets" {
		return runSecrets(path, log)
	}
	if phase == "crds" {
		return runCRDs(log)
	}
	c, err := initjob.Load(path)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	newDatabase := func() (initjob.Database, error) { return initjob.NewPostgres(c.Catalog, c.Tresor.CAFile) }
	if phase != "catalog" {
		return fmt.Errorf("hugr-init: --phase is catalog, secrets or crds, not %q", phase)
	}
	tresor, err := initjob.NewTresor(c.Tresor.URL, c.Tresor.TokenFile, c.Tresor.CAFile)
	if err != nil {
		return err
	}
	var admin initjob.AdminSecret
	if c.Catalog.Admin.SecretName != "" {
		client, err := kubeClient()
		if err != nil {
			return err
		}
		namespace := os.Getenv("POD_NAMESPACE")
		if namespace == "" {
			raw, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
			if err != nil {
				return err
			}
			namespace = string(raw)
		}
		admin = initjob.KubeSecret{Client: client, Namespace: namespace, Name: c.Catalog.Admin.SecretName}
	}
	return initjob.Run(ctx, c, tresor, newDatabase, admin, log)
}

// runSecrets: the umbrella's own parts' secrets, each made once (spec 005 step 3)
func runSecrets(path string, log *slog.Logger) error {
	c, err := initjob.LoadSecrets(path)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	client, err := kubeClient()
	if err != nil {
		return err
	}
	return initjob.RunSecrets(ctx, c, initjob.KubeSecrets{Client: client}, log)
}

func kubeClient() (kubernetes.Interface, error) {
	kube, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(kube)
}

// runCRDs: the release's CRDs, the ones this image carries, applied before an upgrade (spec 008)
func runCRDs(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	manifests, err := crds.Manifests()
	if err != nil {
		return err
	}
	kube, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	client, err := apiextensionsclient.NewForConfig(kube)
	if err != nil {
		return err
	}
	return initjob.RunCRDs(ctx, manifests, initjob.KubeCRDs{Client: client.CustomResourceDefinitions()}, log)
}
