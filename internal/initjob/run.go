package initjob

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
)

// SecretsService is what the Job asks of tresor.
type SecretsService interface {
	Discover(ctx context.Context) error
	Exists(ctx context.Context, name string) (bool, error)
	Create(ctx context.Context, name string, s Secret) error
	Grant(ctx context.Context, name, id, principal string) error
}

// AdminSecret is the database administrator's one-time Secret.
type AdminSecret interface {
	Delete(ctx context.Context) error
}

// Run is the Job: the secret made once (the database's login with it), its grants at every run, the administrator's
// Secret deleted. A run after the secret exists touches neither the database nor the secret.
func Run(ctx context.Context, c *Config, tresor SecretsService, newDatabase func() (Database, error), admin AdminSecret,
	log *slog.Logger) error {
	if err := tresor.Discover(ctx); err != nil {
		return err
	}
	name := c.Catalog.Secret
	exists, err := tresor.Exists(ctx, name)
	if err != nil {
		return err
	}
	if exists {
		log.Info("the catalog's login is in tresor already: the database is left as it is", "secret", name)
	} else {
		db, err := newDatabase()
		if err != nil {
			return err
		}
		password, err := newPassword()
		if err != nil {
			return err
		}
		if err := db.Ensure(ctx, c.Catalog.Login, password, c.Catalog.Database); err != nil {
			return fmt.Errorf("hugr-init: the database: %w", err)
		}
		log.Info("the catalog's database and its login are made", "database", c.Catalog.Database,
			"login", c.Catalog.Login)
		err = tresor.Create(ctx, name, catalogSecret(c, password))
		switch {
		case errors.Is(err, ErrExists):
			// written between the check and now: one Job runs at a time, so this is another install's - the
			// password just set no longer matches it, and the next run cannot tell
			return fmt.Errorf("hugr-init: %s appeared in tresor during the run: another installer writes it; the "+
				"login's password was set and must be written there again (delete the secret and run again)", name)
		case err != nil:
			return fmt.Errorf("hugr-init: tresor: %w", err)
		}
		log.Info("the catalog's login is in tresor", "secret", name)
	}
	for _, principal := range c.Tresor.Grants {
		if err := tresor.Grant(ctx, name, grantID(principal), principal); err != nil {
			return fmt.Errorf("hugr-init: tresor: %w", err)
		}
	}
	log.Info("the catalog's login is granted", "secret", name, "principals", len(c.Tresor.Grants))
	if admin != nil {
		if err := admin.Delete(ctx); err != nil {
			return fmt.Errorf("hugr-init: the database administrator's Secret was not deleted: %w", err)
		}
		log.Info("the database administrator's Secret is deleted", "secret", c.Catalog.Admin.SecretName)
	}
	return nil
}

// RunDatabases is the umbrella's first phase (a pre-install hook, spec 005): each service's database and login, its
// password in a Secret of the service's namespace. A service whose Secret exists is left as it is: its login was made
// by an earlier run, and a password is never changed by itself. The administrator's Secret stays, for the next phase.
func RunDatabases(ctx context.Context, c *Config, newDatabase func() (Database, error), secrets ServiceSecrets,
	log *slog.Logger) error {
	var db Database
	for _, svc := range c.Services {
		exists, err := secrets.Exists(ctx, svc.Secret)
		if err != nil {
			return fmt.Errorf("hugr-init: %s's Secret %s/%s: %w", svc.Name, svc.Secret.Namespace, svc.Secret.Name, err)
		}
		if exists {
			log.Info("a service's database is made already: left as it is", "service", svc.Name)
			continue
		}
		if db == nil {
			if db, err = newDatabase(); err != nil {
				return err
			}
		}
		password, err := newPassword()
		if err != nil {
			return err
		}
		if err := db.Ensure(ctx, svc.Login, password, svc.Database); err != nil {
			return fmt.Errorf("hugr-init: %s's database: %w", svc.Name, err)
		}
		// a failure here leaves the login with a password nobody holds: the next run makes it again (no Secret)
		if err := secrets.Create(ctx, svc.Secret, password); err != nil {
			return fmt.Errorf("hugr-init: %s's Secret %s/%s: %w", svc.Name, svc.Secret.Namespace, svc.Secret.Name, err)
		}
		log.Info("a service's database and login are made", "service", svc.Name, "database", svc.Database,
			"secret", svc.Secret.Namespace+"/"+svc.Secret.Name)
	}
	return nil
}

// catalogSecret is the login as the nodes read it: a postgres secret, the password redacted. Its keys are libpq's own
// (dbname, not database): postgres_scanner takes a CREATE SECRET's aliases there, never a secret's material
func catalogSecret(c *Config, password string) Secret {
	params := map[string]any{
		"host":     c.Catalog.Host,
		"port":     strconv.Itoa(c.Catalog.Port),
		"dbname":   c.Catalog.Database,
		"user":     c.Catalog.Login,
		"password": password,
		"sslmode":  c.Catalog.SSLMode,
	}
	if strings.HasPrefix(c.Catalog.SSLMode, "verify-") && c.Catalog.NodeCAFile != "" {
		params["sslrootcert"] = c.Catalog.NodeCAFile
	}
	return Secret{
		Type:       "postgres",
		Provider:   "config",
		Scope:      []string{},
		Params:     params,
		RedactKeys: []string{"password"},
		Comment:    "the policy catalog's login, made by hugr-init",
	}
}

// newPassword is 32 random bytes, base64url: an alphabet a SQL literal takes as it is
func newPassword() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("hugr-init: no random source: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

var notIDChar = regexp.MustCompile(`[^a-z0-9]+`)

// grantID is the Job's id for a principal's grant: the same at every run, so a run again replaces it. Readable, and
// its hash keeps two principals that read alike (group:A, group:a) apart
func grantID(principal string) string {
	readable := notIDChar.ReplaceAllString(strings.ToLower(principal), "-")
	if len(readable) > 60 {
		readable = readable[:60]
	}
	sum := sha256.Sum256([]byte(principal))
	return "hugr-init-" + readable + "-" + hex.EncodeToString(sum[:4])
}
