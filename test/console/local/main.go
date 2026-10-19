// The console's node calls against real nodes (spec 003), without Kubernetes: the console's own Pool, over TLS with
// the group's name, runs a statement with the token in a file and prints the rows as JSON - what the API answers.
//
//	go run ./test/console/local -node etl=127.0.0.1:41101 -ca ca.crt -token-file token "SELECT * FROM platform.node_load"
package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/hugr-lab/hugr-platform/internal/console"
	"github.com/hugr-lab/hugr-platform/internal/front/fleet"
)

func main() {
	var nodeFlag, caFile, tokenFile string
	flag.StringVar(&nodeFlag, "node", "", "<group>=<host>:<flight port>: the node, dialled with hugr-node-<group>.hugr.svc")
	flag.StringVar(&caFile, "ca", "", "the CA bundle the node's certificate is verified by")
	flag.StringVar(&tokenFile, "token-file", "", "the person's token")
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
	out["signed_out"] = pool.SignOut(context.Background(), strings.TrimSpace(string(token)))
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(out)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(2)
}
