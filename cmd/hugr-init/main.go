// hugr-init is the platform's install Job (spec 005): the policy catalog's database and login, the login into tresor
// as the catalog's secret, its grants to the groups' identities, then the database administrator's Secret deleted.
package main

import (
	"context"
	"flag"
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
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(*config, log); err != nil {
		log.Error("hugr-init failed", "error", err.Error())
		os.Exit(1)
	}
	log.Info("hugr-init done")
}

func run(path string, log *slog.Logger) error {
	c, err := initjob.Load(path)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	tresor, err := initjob.NewTresor(c.Tresor.URL, c.Tresor.TokenFile, c.Tresor.CAFile)
	if err != nil {
		return err
	}
	var admin initjob.AdminSecret
	if c.Catalog.Admin.SecretName != "" {
		kube, err := rest.InClusterConfig()
		if err != nil {
			return err
		}
		client, err := kubernetes.NewForConfig(kube)
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
	return initjob.Run(ctx, c, tresor, func() (initjob.Database, error) {
		return initjob.NewPostgres(c.Catalog, c.Tresor.CAFile)
	}, admin, log)
}
