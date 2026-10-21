package initjob

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Database makes the catalog's database and its login, as the database's administrator.
type Database interface {
	// Ensure makes the login (or sets its password) and the database, and lets the login create its schema there.
	Ensure(ctx context.Context, login, password, database string) error
}

// MinServerVersion is the oldest PostgreSQL the platform's databases run on: 18 (server_version_num).
const MinServerVersion = 180000

// Postgres is the administrator's connection to a PostgreSQL server.
type Postgres struct {
	cfg *pgx.ConnConfig
}

// NewPostgres reads the administrator's credential from its mounted Secret (username, password).
func NewPostgres(c CatalogConfig, caFile string) (*Postgres, error) {
	user, err := readCredential(c.Admin.Dir, "username")
	if err != nil {
		return nil, err
	}
	password, err := readCredential(c.Admin.Dir, "password")
	if err != nil {
		return nil, err
	}
	// the names are checked plain (Validate); the credential is set on the parsed config, never in the string
	dsn := "host=" + c.Host + " port=" + strconv.Itoa(c.Port) + " dbname=" + c.Admin.Database +
		" sslmode=" + c.SSLMode + " connect_timeout=15"
	if caFile != "" && strings.HasPrefix(c.SSLMode, "verify-") {
		dsn += " sslrootcert=" + caFile
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("hugr-init: the database's address: %w", err)
	}
	cfg.User, cfg.Password = user, password
	return &Postgres{cfg: cfg}, nil
}

func readCredential(dir, key string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, key))
	if err != nil {
		return "", fmt.Errorf("hugr-init: the database administrator's %s cannot be read from its Secret", key)
	}
	value := strings.TrimRight(string(raw), "\r\n")
	if value == "" {
		return "", fmt.Errorf("hugr-init: the database administrator's %s is empty", key)
	}
	return value, nil
}

// Ensure makes, or brings back to what the run needs, the login and the database. The database stays the
// administrator's: the login gets CONNECT, CREATE and TEMPORARY on it and USAGE, CREATE on its public schema, and makes
// its own schema or tables there - an administrator of a managed server (Azure, IONOS) may create a role but not set
// the database's owner to it.
func (p *Postgres) Ensure(ctx context.Context, login, password, database string) error {
	conn, err := pgx.ConnectConfig(ctx, p.cfg)
	if err != nil {
		// pgx's error names the host and the user, never the password
		return fmt.Errorf("connecting as the database's administrator: %w", err)
	}
	defer conn.Close(context.Background())
	// the platform's databases are PostgreSQL 18 or later (the owner, 2026-10-21): an older server is refused before
	// anything is made on it
	var version int
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").Scan(&version); err != nil {
		return fmt.Errorf("reading the server's version: %w", err)
	}
	if version < MinServerVersion {
		return fmt.Errorf("the server is PostgreSQL %d.%d: the platform needs %d or later", version/10000,
			version%10000, MinServerVersion/10000)
	}
	role := pgx.Identifier{login}.Sanitize()
	// the server gets the password's SCRAM verifier, never the password: a statement the server logs
	// (log_statement = ddl) carries nothing that logs in. Its alphabet (base64, '$', ':') needs no escaping in a
	// literal, and DDL takes no parameters
	verifier, err := scramVerifier(password)
	if err != nil {
		return err
	}
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", login).Scan(&exists); err != nil {
		return fmt.Errorf("reading the logins: %w", err)
	}
	verb := "CREATE ROLE "
	if exists {
		verb = "ALTER ROLE "
	}
	if _, err := conn.Exec(ctx, verb+role+" WITH LOGIN PASSWORD '"+verifier+"'"); err != nil {
		return fmt.Errorf("the login %s: %s", login, pgCode(err))
	}
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", database).Scan(&exists); err != nil {
		return fmt.Errorf("reading the databases: %w", err)
	}
	db := pgx.Identifier{database}.Sanitize()
	if !exists {
		if _, err := conn.Exec(ctx, "CREATE DATABASE "+db); err != nil {
			return fmt.Errorf("the database %s: %s", database, pgCode(err))
		}
	}
	if _, err := conn.Exec(ctx, "GRANT CONNECT, CREATE, TEMPORARY ON DATABASE "+db+" TO "+role); err != nil {
		return fmt.Errorf("the login's rights on %s: %s", database, pgCode(err))
	}
	// only the database's owner may create in its public schema (PostgreSQL since 15), and a service (tresor, kista)
	// keeps its tables there: the login gets that, in the database itself, its owner staying the administrator
	inner := p.cfg.Copy()
	inner.Database = database
	dbConn, err := pgx.ConnectConfig(ctx, inner)
	if err != nil {
		return fmt.Errorf("connecting to %s as the database's administrator: %w", database, err)
	}
	defer dbConn.Close(context.Background())
	if _, err := dbConn.Exec(ctx, "GRANT USAGE, CREATE ON SCHEMA public TO "+role); err != nil {
		return fmt.Errorf("the login's rights on %s's public schema: %s", database, pgCode(err))
	}
	return nil
}

// pgCode is a failed statement's error without the statement (which holds the verifier): the server's message and
// its code
func pgCode(err error) string {
	type pgError interface{ SQLState() string }
	if e, ok := err.(pgError); ok {
		msg := err.Error()
		if i := strings.Index(msg, "'"); i >= 0 {
			msg = msg[:i] // a message never quotes the password back, but nothing after a quote is kept
		}
		return msg + " (SQLSTATE " + e.SQLState() + ")"
	}
	return "the statement failed"
}

// scramVerifier is PostgreSQL's stored form of a password (RFC 5802/7677, as libpq's PQencryptPasswordConn makes it):
// SCRAM-SHA-256$<iterations>:<salt>$<StoredKey>:<ServerKey>. The password is base64url - SASLprep leaves it as it is
func scramVerifier(password string) (string, error) {
	const iterations = 4096
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("no random source: %w", err)
	}
	salted, err := pbkdf2.Key(sha256.New, password, salt, iterations, sha256.Size)
	if err != nil {
		return "", err
	}
	mac := func(key []byte, msg string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(msg))
		return h.Sum(nil)
	}
	stored := sha256.Sum256(mac(salted, "Client Key"))
	b64 := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", iterations, b64(salt), b64(stored[:]),
		b64(mac(salted, "Server Key"))), nil
}
