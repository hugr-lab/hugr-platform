// The demo's checker (spec 004): SQL on a node as a principal. It takes the principal's token from the IdP by client
// credentials (its secret read from a file, never an argument) and runs a statement through Flight SQL on a node
// behind a port-forward, the TLS name the node's. Rows go to stdout as tab-separated text; a refusal goes to stderr
// with exit code 1. The token is never printed.
//
//	check -token-url https://127.0.0.1:<port>/realms/hugr/protocol/openid-connect/token -token-host keycloak.idp.svc \
//	      -client demo-analyst-a -secret-file <file> -node 127.0.0.1:<port> -server-name hugr-node-a.hugr.svc \
//	      -ca ca.crt -sql 'SELECT count(*) FROM orders'
//
// An IdP in the cluster is reached through a port-forward (-token-host its TLS name, the run's CA); one outside it
// (Entra) directly, by the system's roots, with the -scope it asks for.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/flight/flightsql"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

func main() {
	var (
		tokenURL   = flag.String("token-url", "", "the IdP's token endpoint, reached through a port-forward")
		tokenHost  = flag.String("token-host", "", "the IdP's TLS name behind a port-forward (empty: the IdP is reached directly)")
		scope      = flag.String("scope", "", "the scope the IdP asks for, if any")
		client     = flag.String("client", "", "the principal's client id")
		secretFile = flag.String("secret-file", "", "the file holding the client's secret")
		node       = flag.String("node", "", "the node's Flight address, reached through a port-forward")
		serverName = flag.String("server-name", "", "the node's TLS name (its group's certificate)")
		caFile     = flag.String("ca", "", "the run's CA")
		sql        = flag.String("sql", "", "the statement")
	)
	flag.Parse()
	idp := tokenRequest{url: *tokenURL, host: *tokenHost, client: *client, secretFile: *secretFile, scope: *scope}
	if err := run(idp, *node, *serverName, *caFile, *sql); err != nil {
		fmt.Fprintln(os.Stderr, redact(err.Error()))
		os.Exit(1)
	}
}

var jwt = regexp.MustCompile(`eyJ[A-Za-z0-9._-]*`)

// redact blanks a token wherever it stands in a message: quoted, after a "=", inside a word
func redact(text string) string { return jwt.ReplaceAllString(text, "<token>") }

// tokenRequest: where and as whom the principal's token is asked for
type tokenRequest struct {
	url, host, client, secretFile, scope string
}

func run(idp tokenRequest, node, serverName, caFile, sql string) error {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return errors.New("the CA file holds no certificate")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	token, err := clientCredentials(ctx, idp, pool)
	if err != nil {
		return fmt.Errorf("the principal's token: %w", err)
	}
	creds := credentials.NewTLS(&tls.Config{RootCAs: pool, ServerName: serverName, MinVersion: tls.VersionTLS12})
	conn, err := flightsql.NewClient(node, nil, nil, grpc.WithTransportCredentials(creds))
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	info, err := conn.Execute(ctx, sql)
	if err != nil {
		return err
	}
	for _, endpoint := range info.Endpoint {
		reader, err := conn.DoGet(ctx, endpoint.Ticket)
		if err != nil {
			return err
		}
		for reader.Next() {
			record := reader.RecordBatch()
			for row := 0; row < int(record.NumRows()); row++ {
				cells := make([]string, record.NumCols())
				for col := range cells {
					column := record.Column(col)
					if column.IsNull(row) {
						cells[col] = "NULL"
						continue
					}
					cells[col] = valueString(column, row)
				}
				fmt.Println(strings.Join(cells, "\t"))
			}
		}
		if err := reader.Err(); err != nil {
			reader.Release()
			return err
		}
		reader.Release()
	}
	return nil
}

func valueString(column arrow.Array, row int) string {
	return column.ValueStr(row)
}

// clientCredentials: the principal's token by its client's secret; the secret is read from its file only
func clientCredentials(ctx context.Context, idp tokenRequest, pool *x509.CertPool) (string, error) {
	secret, err := os.ReadFile(idp.secretFile)
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {idp.client},
		"client_secret": {strings.TrimSpace(string(secret))}}
	if idp.scope != "" {
		form.Set("scope", idp.scope)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, idp.url, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	config := &tls.Config{MinVersion: tls.VersionTLS12}
	if idp.host != "" { // the cluster's IdP behind a port-forward: the run's CA, its own name
		config.RootCAs, config.ServerName = pool, idp.host
	}
	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: config, Proxy: http.ProxyFromEnvironment}}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var answer struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		return "", fmt.Errorf("the IdP answered %d, not JSON: %w", resp.StatusCode, err)
	}
	if answer.AccessToken == "" {
		return "", fmt.Errorf("the IdP answered %d %s", resp.StatusCode, answer.Error)
	}
	return answer.AccessToken, nil
}
