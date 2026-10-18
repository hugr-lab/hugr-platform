#!/usr/bin/env bash
# The platform in one cluster (spec 004): the operator, two groups of hugr-node pods, the IdP logging the nodes in by
# their ServiceAccount tokens, tresor-server, kista and PostgreSQL - and the checks of what spec 001 and hugr-node
# spec 010 promise.
#
#   test/demo/run.sh minikube [up | check | down]   the main run, on a developer's machine (the profile hugr-demo)
#   test/demo/run.sh kind                           CI: makes the cluster, runs everything, deletes it
#   test/demo/run.sh aks [up | check | down | leftovers]   by hand: one run makes and deletes everything of Azure
#
# Without a command a run does up and check (and, on kind and aks, down). The run's state - its CA, its secrets - is in
# ~/.cache/hugr-platform-demo/<provider> (mode 0700); nothing of a token or a secret is printed.
#   HUGR_NODE_DIST    the node's release assets (dist-linux_<arch>) instead of hugr-node's latest release dry run
#   NODE_IMAGE        a published hugr-node image instead of both
#   TRESOR_SERVER_DIR, KISTA_DIR, HUGR_NODE_DIR   checkouts instead of each one's main
#   HUGR_DEMO_ONLY    up to a step: images, db, idp, tresor, kista, seed, operator, platform (for working on one)
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
HERE="$ROOT/test/demo"
export GOWORK=off
PROVIDER="${1:-}"
COMMAND="${2:-all}"
case "$PROVIDER" in
kind | minikube | aks) ;;
*)
	sed -n '6,9p' "$0" >&2
	exit 2
	;;
esac
CACHE="${HUGR_DEMO_CACHE:-$HOME/.cache/hugr-platform-demo}"
WORK="$CACHE/$PROVIDER"
mkdir -p "$WORK" && chmod 700 "$WORK"
PIDS=()
trap 'for p in ${PIDS[@]+"${PIDS[@]}"}; do kill "$p" 2>/dev/null || true; done' EXIT

. "$HERE/lib/common.sh"
. "$HERE/lib/images.sh"
. "$HERE/lib/stack.sh"
. "$HERE/lib/seed.sh"
. "$HERE/lib/platform.sh"
. "$HERE/lib/checks.sh"
. "$HERE/lib/provider-$PROVIDER.sh"

need docker kubectl helm go gh openssl python3 git
provider_need

stage() { # <name>: false once past HUGR_DEMO_ONLY
	[ -z "${DONE_ONLY:-}" ] || return 1
	[ "${HUGR_DEMO_ONLY:-}" != "$1" ] || DONE_ONLY=1
	return 0
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
check) run_checks ;;
down) provider_down ;;
all)
	if [ "$PROVIDER" = minikube ]; then
		up
		[ -n "${DONE_ONLY:-}" ] || run_checks
	else
		trap 'provider_down; for p in ${PIDS[@]+"${PIDS[@]}"}; do kill "$p" 2>/dev/null || true; done' EXIT
		up
		[ -n "${DONE_ONLY:-}" ] || run_checks
	fi
	;;
*)
	echo "the command is up, check, down or nothing" >&2
	exit 2
	;;
esac
