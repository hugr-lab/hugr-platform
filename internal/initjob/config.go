// Package initjob is the platform's install Job, hugr-init (spec 005): it makes the policy catalog's database and its
// login, writes the login into tresor as hugr_policy_catalog, and grants it to the groups' identities. It holds no
// credential beyond its run: the database administrator's comes once in a Secret it deletes after use, the login's
// password is made here and goes to tresor only, and tresor takes the Job's projected ServiceAccount token.
package initjob

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"
)

// Config is the Job's configuration (a ConfigMap the chart renders): names and addresses, never a credential.
type Config struct {
	Tresor  TresorConfig  `json:"tresor"`
	Catalog CatalogConfig `json:"catalog"`
}

// TresorConfig is where the secret goes and who may use it.
type TresorConfig struct {
	// URL is tresor-server's base: its discovery is <url>/.well-known/duckdb-secrets.
	URL string `json:"url"`
	// TokenFile is the Job's projected ServiceAccount token, read at each request.
	TokenFile string `json:"tokenFile"`
	// CAFile verifies tresor's certificate (and the database's, with a verify sslmode); empty = the system roots.
	CAFile string `json:"caFile,omitempty"`
	// Grants are the principals given use of the secret: the groups' identities in tresor (role:… or group:…).
	Grants []string `json:"grants"`
}

// CatalogConfig is the policy catalog's database and its login.
type CatalogConfig struct {
	// Secret is the name the nodes find the catalog by (HugrPlatform.spec.catalog.secret).
	Secret string `json:"secret"`
	// Engine is the database's kind: postgres (stage A).
	Engine   string `json:"engine"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Database string `json:"database"`
	Login    string `json:"login"`
	// SSLMode of the administrator's connection: disable, require, verify-ca or verify-full.
	SSLMode string `json:"sslmode"`
	// Admin is the database administrator's one-time credential.
	Admin AdminConfig `json:"admin"`
}

// AdminConfig is the database administrator's credential: a Secret of the Job's namespace, mounted as files
// (username, password), deleted after a run that made what it was for.
type AdminConfig struct {
	SecretName string `json:"secretName"`
	Dir        string `json:"dir"`
	// Database the administrator connects to (postgres by default).
	Database string `json:"database,omitempty"`
}

var (
	// a database object's name goes into DDL as an identifier: plain names only
	plainName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)
	// a grant names a role or a group of tresor (its protocol: nothing else is granted)
	principal = regexp.MustCompile(`^(role|group):[^\s]+$`)
	hostName  = regexp.MustCompile(`^[A-Za-z0-9.-]+$`)
)

// Load reads and checks the configuration.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("hugr-init: the configuration: %w", err)
	}
	var c Config
	if err := yaml.UnmarshalStrict(raw, &c); err != nil {
		return nil, fmt.Errorf("hugr-init: the configuration: %w", err)
	}
	if c.Catalog.Port == 0 {
		c.Catalog.Port = 5432
	}
	if c.Catalog.Admin.Database == "" {
		c.Catalog.Admin.Database = "postgres"
	}
	if c.Catalog.SSLMode == "" {
		c.Catalog.SSLMode = "verify-full"
	}
	return &c, c.Validate()
}

// Validate refuses what the Job cannot run on, naming the field.
func (c *Config) Validate() error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	if !strings.HasPrefix(c.Tresor.URL, "https://") || strings.Contains(c.Tresor.URL, "@") {
		add("tresor.url is an https URL without userinfo")
	}
	if c.Tresor.TokenFile == "" {
		add("tresor.tokenFile is required")
	}
	if len(c.Tresor.Grants) == 0 {
		add("tresor.grants names no principal: the groups' identities could not use the catalog")
	}
	for _, p := range c.Tresor.Grants {
		if !principal.MatchString(p) {
			add("tresor.grants: %q is not a role:… or group:… principal", p)
		}
	}
	if c.Catalog.Secret == "" || len(c.Catalog.Secret) > 200 || strings.TrimSpace(c.Catalog.Secret) != c.Catalog.Secret {
		add("catalog.secret is a name of at most 200 characters")
	}
	if c.Catalog.Engine != "postgres" {
		add("catalog.engine %q is not one this Job makes (postgres)", c.Catalog.Engine)
	}
	if !hostName.MatchString(c.Catalog.Host) {
		add("catalog.host is a host name")
	}
	if c.Catalog.Port < 1 || c.Catalog.Port > 65535 {
		add("catalog.port is 1..65535")
	}
	if !plainName.MatchString(c.Catalog.Database) {
		add("catalog.database is a plain name ([a-z_][a-z0-9_]*)")
	}
	if !plainName.MatchString(c.Catalog.Login) {
		add("catalog.login is a plain name ([a-z_][a-z0-9_]*)")
	}
	if !plainName.MatchString(c.Catalog.Admin.Database) {
		add("catalog.admin.database is a plain name")
	}
	switch c.Catalog.SSLMode {
	case "disable", "require", "verify-ca", "verify-full":
	default:
		add("catalog.sslmode is disable, require, verify-ca or verify-full")
	}
	if c.Catalog.Admin.Dir == "" {
		add("catalog.admin.dir is required: the administrator's Secret, mounted")
	}
	if len(problems) > 0 {
		return fmt.Errorf("hugr-init: the configuration: %s", strings.Join(problems, "; "))
	}
	return nil
}
