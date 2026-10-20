// The console's node calls against real nodes (spec 003), without Kubernetes: the console's own Pool, over TLS with
// the group's name, runs a statement with the token in a file and prints the rows as JSON - what the API answers.
// With -keep it holds a session of the person's own instead, as a driver does.
//
//	go run ./test/console/local -node etl=127.0.0.1:41101 -ca ca.crt -token-file token "SELECT * FROM platform.node_load"
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/apache/arrow-go/v18/arrow/flight"
	"github.com/apache/arrow-go/v18/arrow/flight/flightsql"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"

	"github.com/hugr-lab/hugr-platform/internal/console"
	"github.com/hugr-lab/hugr-platform/internal/front/fleet"
)

func main() {
	var nodeFlag, caFile, tokenFile string
	var keep bool
	flag.StringVar(&nodeFlag, "node", "", "<group>=<host>:<flight port>: the node, dialled with hugr-node-<group>.hugr.svc")
	flag.StringVar(&caFile, "ca", "", "the CA bundle the node's certificate is verified by")
	flag.StringVar(&tokenFile, "token-file", "", "the person's token")
	flag.BoolVar(&keep, "keep", false, "hold a session of the person's own, as a driver does (the node's cookie kept), "+
		"and leave it open: an e2e's other session")
	flag.Parse()
	group, address, ok := strings.Cut(nodeFlag, "=")
	host, port, ok2 := strings.Cut(address, ":")
	p, err := strconv.Atoi(port)
	if !ok || !ok2 || err != nil {
		fail(fmt.Errorf("-node is <group>=<host>:<port>"))
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		fail(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pem)
	token, err := os.ReadFile(tokenFile)
	if err != nil {
		fail(err)
	}
	if keep {
		// the console keeps no session on a node (each of its calls is one of its own): a session to look at is a
		// driver's, which keeps the door's cookie
		if err := hold(address, "hugr-node-"+group+".hugr.svc", roots, strings.TrimSpace(string(token)), flag.Args()); err != nil {
			fail(err)
		}
		return
	}
	pool := &console.Pool{Namespace: "hugr", Roots: func(context.Context) (*x509.CertPool, error) { return roots, nil }}
	n := fleet.Node{Label: group + "-node", Service: "hugr-node-" + group, Address: host, FlightPort: int32(p)}
	out := map[string]any{}
	for _, statement := range flag.Args() {
		result, err := pool.Query(context.Background(), n, strings.TrimSpace(string(token)), statement)
		if err != nil {
			answer := map[string]string{"error": err.Error()}
			var refused *console.NodeError
			if errors.As(err, &refused) {
				answer["code"] = refused.Code.String()
			}
			out[statement] = answer
			continue
		}
		out[statement] = result.Records()
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(out)
}

// hold runs the statements on one connection that keeps the node's cookie, and leaves its session open.
func hold(address, serverName string, roots *x509.CertPool, token string, statements []string) error {
	creds := credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: serverName})
	client, err := flightsql.NewClient(address, nil, []flight.ClientMiddleware{flight.NewClientCookieMiddleware()},
		grpc.WithTransportCredentials(creds))
	if err != nil {
		return err
	}
	defer client.Close()
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+token)
	out := map[string]int64{}
	for _, statement := range statements {
		info, err := client.Execute(ctx, statement)
		if err != nil {
			return fmt.Errorf("%s: %w", statement, err)
		}
		var rows int64
		for _, endpoint := range info.Endpoint {
			reader, err := client.DoGet(ctx, endpoint.Ticket)
			if err != nil {
				return fmt.Errorf("%s: %w", statement, err)
			}
			for reader.Next() {
				rows += reader.RecordBatch().NumRows()
			}
			reader.Release()
		}
		out[statement] = rows
	}
	return json.NewEncoder(os.Stdout).Encode(out)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(2)
}
