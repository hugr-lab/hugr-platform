// The front without Kubernetes, for test/front/run.sh (spec 002): the fleet is the nodes named on the command line,
// each current and admitting, in no group; both doors in the clear on localhost. The proxies are the front's own.
//
//	local -flight :41010 -quack :41494 -node node-1=127.0.0.1:41011:41495 -node node-2=127.0.0.1:41012:41496
package main

import (
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hugr-lab/hugr-platform/internal/front/fleet"
	"github.com/hugr-lab/hugr-platform/internal/front/flightproxy"
	"github.com/hugr-lab/hugr-platform/internal/front/quackproxy"
	"github.com/hugr-lab/hugr-platform/pkg/nodeproto"
)

type nodes []fleet.Node

func (n *nodes) String() string { return "" }

// Set one node: <label>=<host>:<flight port>:<quack port>
func (n *nodes) Set(v string) error {
	label, address, _ := strings.Cut(v, "=")
	parts := strings.Split(address, ":")
	if len(parts) != 3 {
		return flag.ErrHelp
	}
	flight, _ := strconv.Atoi(parts[1])
	quack, _ := strconv.Atoi(parts[2])
	*n = append(*n, fleet.Node{Label: label, Service: "local", Address: parts[0], FlightPort: int32(flight),
		QuackPort: int32(quack), Ready: true, Serving: true, Lease: &fleet.Lease{Until: time.Now().Add(24 * time.Hour),
			Summary: nodeproto.Summary{AdmitSession: true, SeatsLeft: 10}}})
	return nil
}

func main() {
	var list nodes
	flightAddr := flag.String("flight", "127.0.0.1:41010", "the Flight door")
	quackAddr := flag.String("quack", "127.0.0.1:41494", "the quack door")
	flag.Var(&list, "node", "a node: <label>=<host>:<flight port>:<quack port> (repeated)")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	f := fleet.New()
	f.Set(fleet.NewView(list))
	flight := &flightproxy.Proxy{Fleet: f, NoNodeWait: time.Second, Log: log, Insecure: true}
	quack := &quackproxy.Proxy{Fleet: f, NoNodeWait: time.Second, Log: log, Insecure: true}
	l, err := net.Listen("tcp", *flightAddr)
	if err != nil {
		log.Error("listen", "error", err.Error())
		os.Exit(1)
	}
	go func() { _ = flight.Server(nil).Serve(l) }()
	server := &http.Server{Addr: *quackAddr, Handler: quack.Handler(), ReadHeaderTimeout: 10 * time.Second}
	log.Info("serving", "flight", *flightAddr, "quack", *quackAddr, "nodes", len(list))
	if err := server.ListenAndServe(); err != nil {
		log.Error("serve", "error", err.Error())
		os.Exit(1)
	}
}
