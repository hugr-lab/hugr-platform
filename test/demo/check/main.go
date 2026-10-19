// The demo's checker (spec 004): SQL on a node as a principal. It takes the principal's token from the IdP by client
// credentials (its secret read from a file, never an argument) and runs a statement through Flight SQL on a node
// behind a port-forward, the TLS name the node's. Rows go to stdout as tab-separated text; a refusal goes to stderr
// with exit code 1. The token is never printed. With -hold the call keeps the node's cookie and leaves its session
// open, as a driver's connection does; with -console it calls the console's API (spec 003) instead.
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
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/flight"
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
		hold       = flag.Bool("hold", false, "keep the node's cookie and leave the session open: a client's own session")
		consoleURL = flag.String("console", "", "instead of -sql: a console API URL (spec 003), reached through a port-forward")
		method     = flag.String("method", "GET", "the console call's method")
		body       = flag.String("body", "", "the console call's JSON body")
	)
	flag.Parse()
	idp := tokenRequest{url: *tokenURL, host: *tokenHost, client: *client, secretFile: *secretFile, scope: *scope}
	var err error
	if *consoleURL != "" {
		err = console(idp, *consoleURL, *method, *body, *serverName, *caFile)
	} else {
		err = run(idp, *node, *serverName, *caFile, *sql, *hold)
	}
	if err != nil {
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

func run(idp tokenRequest, node, serverName, caFile, sql string, hold bool) error {
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
	// without the cookie each call is a session of its own, closed after it (duckdb-acl spec 050); with it the DoGet
	// seats the session the node keeps until its idle timeout - nothing here closes it
	var middleware []flight.ClientMiddleware
	if hold {
		middleware = append(middleware, flight.NewClientCookieMiddleware())
	}
	conn, err := flightsql.NewClient(node, nil, middleware, grpc.WithTransportCredentials(creds))
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

// console calls the console's API as the principal (spec 003): the answer's body to stdout, a refusal's to stderr with
// exit code 1; the token is the bearer and never printed.
func console(idp tokenRequest, target, method, body, serverName, caFile string) error {
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
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12,
		RootCAs: pool, ServerName: serverName}}}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("the console answered %d: %s", resp.StatusCode, strings.TrimSpace(string(answer)))
	}
	if strings.Contains(string(answer), token) {
		return errors.New("the console's answer carries the token")
	}
	fmt.Println(strings.TrimSpace(string(answer)))
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
