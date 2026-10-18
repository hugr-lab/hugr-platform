// The demo's checker (spec 004): SQL on a node as a principal. It takes the principal's token from the IdP by client
// credentials (its secret read from a file, never an argument) and runs a statement through Flight SQL on a node
// behind a port-forward, the TLS name the node's. Rows go to stdout as tab-separated text; a refusal goes to stderr
// with exit code 1. The token is never printed.
//
//	check -token-url https://127.0.0.1:<port>/realms/hugr/protocol/openid-connect/token -token-host keycloak.idp.svc \
//	      -client demo-analyst-a -secret-file <file> -node 127.0.0.1:<port> -server-name hugr-node-a.hugr.svc \
//	      -ca ca.crt -sql 'SELECT count(*) FROM orders'
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
		tokenHost  = flag.String("token-host", "", "the IdP's TLS name")
		client     = flag.String("client", "", "the principal's client id")
		secretFile = flag.String("secret-file", "", "the file holding the client's secret")
		node       = flag.String("node", "", "the node's Flight address, reached through a port-forward")
		serverName = flag.String("server-name", "", "the node's TLS name (its group's certificate)")
		caFile     = flag.String("ca", "", "the run's CA")
		sql        = flag.String("sql", "", "the statement")
	)
	flag.Parse()
	if err := run(*tokenURL, *tokenHost, *client, *secretFile, *node, *serverName, *caFile, *sql); err != nil {
		fmt.Fprintln(os.Stderr, redact(err.Error()))
		os.Exit(1)
	}
}

func redact(text string) string {
	words := strings.Fields(text)
	for i, w := range words {
		if strings.HasPrefix(w, "eyJ") {
			words[i] = "<token>"
		}
	}
	return strings.Join(words, " ")
}

func run(tokenURL, tokenHost, client, secretFile, node, serverName, caFile, sql string) error {
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
	token, err := clientCredentials(ctx, tokenURL, tokenHost, client, secretFile, pool)
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
func clientCredentials(ctx context.Context, tokenURL, tokenHost, client, secretFile string, pool *x509.CertPool) (string, error) {
	secret, err := os.ReadFile(secretFile)
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {client},
		"client_secret": {strings.TrimSpace(string(secret))}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool,
		ServerName: tokenHost, MinVersion: tls.VersionTLS12}}}
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
		return "", err
	}
	if answer.AccessToken == "" {
		return "", fmt.Errorf("the IdP answered %d %s", resp.StatusCode, answer.Error)
	}
	return answer.AccessToken, nil
}
