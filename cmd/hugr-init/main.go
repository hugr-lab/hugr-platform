// hugr-init is the platform's install Job (spec 005): the policy catalog's database and login, the login into tresor
// as the catalog's secret, its grants to the groups' identities, then the database administrator's Secret deleted.
// --phase databases (the umbrella's pre-install hook) makes the services' (tresor's, kista's) databases first.
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

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/hugr-lab/hugr-platform/internal/initjob"
)

func main() {
	config := flag.String("config", "/etc/hugr-init/config.yaml", "the Job's configuration")
	phase := flag.String("phase", "catalog", "catalog (the catalog's secret, its grants) or databases (the services')")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(*config, *phase, log); err != nil {
		log.Error("hugr-init failed", "error", err.Error())
		os.Exit(1)
	}
	log.Info("hugr-init done")
}

func run(path, phase string, log *slog.Logger) error {
	c, err := initjob.Load(path)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	newDatabase := func() (initjob.Database, error) { return initjob.NewPostgres(c.Catalog, c.Tresor.CAFile) }
	switch phase {
	case "databases":
		client, err := kubeClient()
		if err != nil {
			return err
		}
		return initjob.RunDatabases(ctx, c, newDatabase, initjob.KubeServiceSecrets{Client: client}, log)
	case "catalog":
	default:
		return fmt.Errorf("hugr-init: --phase is catalog or databases, not %q", phase)
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

func kubeClient() (kubernetes.Interface, error) {
	kube, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(kube)
}
