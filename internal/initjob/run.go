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
	"slices"
	"strconv"
	"strings"
)

// SecretsService is what the Job asks of tresor.
type SecretsService interface {
	Discover(ctx context.Context) error
	Exists(ctx context.Context, name string) (bool, error)
	Describe(ctx context.Context, name string) (*Descriptor, error)
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
	// the database administrator's credential goes as soon as the catalog is done: the doors need none of it
	if admin != nil {
		if err := admin.Delete(ctx); err != nil {
			return fmt.Errorf("hugr-init: the database administrator's Secret was not deleted: %w", err)
		}
		log.Info("the database administrator's Secret is deleted", "secret", c.Catalog.Admin.SecretName)
	}
	return door(ctx, c, tresor, log)
}

// door writes the doors' secret once and grants its use at every run, as the catalog's. An existing one (the
// administrator's, an earlier run's) is left as it is - granted only while it is a quack secret within the doors
// named: a stale one with a broader scope, or another secret by that name, is never handed to people.
func door(ctx context.Context, c *Config, tresor SecretsService, log *slog.Logger) error {
	name := c.Door.Secret
	existing, err := tresor.Describe(ctx, name)
	if err != nil {
		return err
	}
	if existing != nil {
		if problem := outsideDoors(existing, c.Door.Scopes); problem != "" {
			return fmt.Errorf("hugr-init: %s in tresor %s: nothing granted - delete it, or name its doors in "+
				"door.scopes", name, problem)
		}
		log.Info("the doors' secret is in tresor already: left as it is", "secret", name)
	} else {
		err := tresor.Create(ctx, name, doorSecret(c))
		switch {
		case errors.Is(err, ErrExists):
			// written meanwhile, or an entry the listing does not show (a service login): judged as any existing one
			existing, err = tresor.Describe(ctx, name)
			if err != nil {
				return err
			}
			if existing == nil {
				return fmt.Errorf("hugr-init: %s is taken in tresor by an entry its listing does not show: nothing "+
					"granted - name the doors' secret otherwise (door.secret)", name)
			}
			if problem := outsideDoors(existing, c.Door.Scopes); problem != "" {
				return fmt.Errorf("hugr-init: %s in tresor %s: nothing granted - delete it, or name its doors in "+
					"door.scopes", name, problem)
			}
		case err != nil:
			return fmt.Errorf("hugr-init: tresor: %w", err)
		default:
			log.Info("the doors' secret is in tresor", "secret", name, "doors", len(c.Door.Scopes))
		}
	}
	for _, principal := range c.Door.Grants {
		if err := tresor.Grant(ctx, name, grantID(principal), principal); err != nil {
			return fmt.Errorf("hugr-init: tresor: %w", err)
		}
	}
	log.Info("the doors' secret is granted", "secret", name, "principals", len(c.Door.Grants))
	return nil
}

// outsideDoors says how an existing secret is not the doors' (empty: it is): another type, or a scope beyond them.
func outsideDoors(d *Descriptor, doors []string) string {
	if d.Type != "quack" {
		return fmt.Sprintf("is of type %q, not quack", d.Type)
	}
	if len(d.Scope) == 0 {
		return "has no scope: it would go to every quack server"
	}
	if !d.Dynamic {
		return "carries a fixed value, not one made for its reader: one credential would go to everyone granted"
	}
	for _, scope := range d.Scope {
		if !slices.Contains(doors, scope) {
			return fmt.Sprintf("names %q, which door.scopes does not", scope)
		}
	}
	return ""
}

// doorSecret is a quack secret of no value of its own: tresor mints its token for the reader, for the nodes'
// audience, at each read (tresor-server's generated parameters) - as the door's token, and as the bearer the
// console's /quack admits a signed-in person by (spec 007). One exchange makes both.
func doorSecret(c *Config) Secret {
	return Secret{
		Type:     "quack",
		Provider: "config",
		Scope:    append([]string{}, c.Door.Scopes...),
		Params: map[string]any{
			"token": "mint+exchange://" + c.Door.Audience,
			"extra_http_headers": map[string]any{"type": "MAP(VARCHAR, VARCHAR)",
				"value": map[string]string{"Authorization": "Bearer ${exchange://" + c.Door.Audience + "}"}},
		},
		RedactKeys: []string{"token", "extra_http_headers"},
		Comment:    "the platform's quack doors: a token minted for the reader, made by hugr-init (spec 009)",
	}
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
