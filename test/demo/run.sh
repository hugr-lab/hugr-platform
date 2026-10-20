#!/usr/bin/env bash
# The platform in one cluster (spec 004): the operator, two groups of hugr-node pods, the IdP logging the nodes in by
# their ServiceAccount tokens, tresor-server, kista and PostgreSQL - and the checks of what spec 001 and hugr-node
# spec 010 promise.
#
#   test/demo/run.sh minikube [up | check | down]   the main run, on a developer's machine (the profile hugr-demo)
#   test/demo/run.sh kind                           CI: makes the cluster, runs everything, deletes it
#   test/demo/run.sh aks [up | check | down | leftovers]   by hand: one run makes and deletes everything of Azure
#   test/demo/run.sh <provider> diagnose            the cluster's state, through the run's redaction (CI's on a failure)
#   test/demo/run.sh <kind | minikube> console      the console for a person's browser, after an up (lib/stand.sh)
#
# The IdP is Keycloak in the cluster on kind and minikube, Entra ID on AKS. The checks change the profile: they run
# once per up (a run without a command does both).
#
# Without a command a run does up and check (and, on kind and aks, down). The run's state - its CA, its secrets - is in
# ~/.cache/hugr-platform-demo/<provider> (mode 0700); nothing of a token or a secret is printed.
#   HUGR_NODE_DIST    the node's release assets (dist-linux_<arch>) instead of hugr-node's latest release dry run
#   NODE_IMAGE        a published hugr-node image instead of the dry run's (the extensions still come from the dry run
#                     or HUGR_NODE_DIST: they must be the image's duckdb's)
#   TRESOR_SERVER_DIR, KISTA_DIR, HUGR_NODE_DIR   checkouts instead of each one's main
#   HUGR_DEMO_ONLY    up to a step: images, db, idp, tresor, kista, seed, operator, platform (for working on one)
#   HUGR_DEMO_NODE_CPU  a node's CPU request (1; CI's 4-core runner: 250m - the node rounds it up to 1 thread)
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
HERE="$ROOT/test/demo"
export GOWORK=off
PROVIDER="${1:-}"
COMMAND="${2:-all}"
case "$PROVIDER" in
kind | minikube | aks) ;;
*)
	sed -n '6,10p' "$0" >&2
	exit 2
	;;
esac
CACHE="${HUGR_DEMO_CACHE:-$HOME/.cache/hugr-platform-demo}"
WORK="$CACHE/$PROVIDER"
mkdir -p "$WORK" && chmod 700 "$WORK"
trap 'stop_forwards' EXIT

. "$HERE/lib/common.sh"
. "$HERE/lib/images.sh"
. "$HERE/lib/stack.sh"
. "$HERE/lib/seed.sh"
. "$HERE/lib/platform.sh"
. "$HERE/lib/checks.sh"
. "$HERE/lib/stand.sh"
. "$HERE/lib/provider-$PROVIDER.sh"
if [ "$PROVIDER" = aks ]; then
	. "$HERE/lib/idp-entra.sh"
else
	. "$HERE/lib/idp-keycloak.sh"
fi

need docker kubectl helm go gh openssl python3 git
provider_need

stage() { # <name>: false once the step HUGR_DEMO_ONLY names is done
	[ -z "${DONE_ONLY:-}" ] || return 1
	[ "${HUGR_DEMO_ONLY:-}" != "$1" ] || {
		DONE_ONLY=1
		return 1
	}
}

up() {
	provider_up
	build_images
	TRESOR_SRC="$(src tresor-server TRESOR_SERVER_DIR)"
	KISTA_SRC="$(src duckdb-extension-repository KISTA_DIR)"
	ca_make
	stage images || return 0
	install_db
	stage db || return 0
	install_idp
	stage idp || return 0
	install_tresor
	stage tresor || return 0
	install_kista
	stage kista || return 0
	seed
	stage seed || return 0
	install_operator
	stage operator || return 0
	install_platform
	stage platform || return 0
}

case "$COMMAND" in
up) up ;;
check)
	provider_context
	run_checks
	;;
down) provider_down ;;
diagnose)
	provider_context
	diagnose
	;;
console)
	provider_context
	console_stand
	;;
leftovers)
	[ "$PROVIDER" = aks ] || {
		echo "leftovers is AKS's: kind and minikube keep nothing outside this machine" >&2
		exit 2
	}
	provider_leftovers
	;;
all)
	if [ "$PROVIDER" = minikube ]; then
		up
		[ -n "${DONE_ONLY:-}" ] || run_checks
	else
		trap 'stop_forwards; provider_down' EXIT
		up
		[ -n "${DONE_ONLY:-}" ] || run_checks
	fi
	;;
*)
	echo "the command is up, check, down, diagnose, console, leftovers (aks) or nothing" >&2
	exit 2
	;;
esac
