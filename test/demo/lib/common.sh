# shellcheck shell=bash
# The demo's shared helpers (spec 004): steps and checks, the run's CA, kubectl on the demo's context only.

step() { printf '\n== %s\n' "$*"; }
ok() { echo "ok: $*"; }
fail() {
	echo "FAIL: $*" >&2
	exit 1
}
skip() {
	echo "SKIP: $*"
	exit 0
}
# a token never reaches the terminal: a JWT and a bearer header are blanked
redact() { sed -E -e 's/eyJ[A-Za-z0-9._-]*/<token>/g' -e 's/(Bearer )[A-Za-z0-9._~+\/=-]+/\1<token>/g'; }

need() {
	local tool
	for tool in "$@"; do
		command -v "$tool" >/dev/null || {
			[ -n "${CI:-}" ] && fail "$tool not found" || skip "$tool not found"
		}
	done
}

# kubectl on the demo's cluster only - never the caller's current context
k() { command kubectl --context "$DEMO_CONTEXT" "$@"; }
helm_() { command helm --kube-context "$DEMO_CONTEXT" "$@"; }

# eventually <seconds> <what> <command...>: the command succeeds within the time, else the run fails naming what. The
# command runs afresh at every try: a condition that reads state is a function (status_is, lease_is), never a test of
# a $(...) the caller's shell expanded once
eventually() {
	local within="$1" what="$2"
	shift 2
	local deadline=$((SECONDS + within))
	while [ "$SECONDS" -lt "$deadline" ]; do
		"$@" >/dev/null 2>&1 && return 0
		sleep 2
	done
	"$@" || fail "not within ${within}s: $what"
}

# rollout <namespace> <deployment>: it serves, or its pods' state and logs say why not
rollout() {
	if ! k -n "$1" rollout status "deploy/$2" --timeout "${3:-300s}" >/dev/null; then
		k -n "$1" get pods -o wide >&2 || true
		k -n "$1" logs "deploy/$2" --tail 80 2>&1 | redact >&2 || true
		fail "$1/$2 did not roll out"
	fi
}

# --- the run's CA: every https server here has a certificate of it ---------------------------------------------------
ca_make() {
	[ -f "$WORK/ca.crt" ] && return 0
	openssl req -x509 -newkey rsa:2048 -nodes -keyout "$WORK/ca.key" -out "$WORK/ca.crt" -days 2 \
		-subj "/CN=hugr-platform demo CA" 2>/dev/null
}

# ca_cert <name> <dns>...: a server certificate in $WORK/<name>.{crt,key}, also for 127.0.0.1 and localhost (a
# port-forward reaches it there)
ca_cert() {
	local name="$1" san="IP:127.0.0.1,DNS:localhost" dns
	shift
	for dns in "$@"; do san="$san,DNS:$dns"; done
	openssl req -newkey rsa:2048 -nodes -keyout "$WORK/$name.key" -out "$WORK/$name.csr" -subj "/CN=$1" 2>/dev/null
	printf 'subjectAltName=%s\nextendedKeyUsage=serverAuth\n' "$san" >"$WORK/$name.ext"
	openssl x509 -req -in "$WORK/$name.csr" -CA "$WORK/ca.crt" -CAkey "$WORK/ca.key" -CAcreateserial -days 2 \
		-extfile "$WORK/$name.ext" -out "$WORK/$name.crt" 2>/dev/null
}

# tls_secret <namespace> <name> <cert name>: a kubernetes.io/tls Secret, made again
tls_secret() {
	k -n "$1" delete secret "$2" --ignore-not-found >/dev/null
	k -n "$1" create secret tls "$2" --cert "$WORK/$3.crt" --key "$WORK/$3.key" >/dev/null
}

namespace() { k get namespace "$1" >/dev/null 2>&1 || k create namespace "$1" >/dev/null; }

# secret_value <name>: a value of the run's, made once and kept in its state (mode 0600), never printed
secret_value() {
	local file="$WORK/secrets/$1"
	if [ ! -f "$file" ]; then
		mkdir -p "$WORK/secrets" && chmod 700 "$WORK/secrets"
		openssl rand -hex 24 >"$file"
		chmod 600 "$file"
	fi
	cat "$file"
}

# secret_b64 <name> <bytes>: a key of the run's, base64, made once and kept like secret_value
secret_b64() {
	local file="$WORK/secrets/$1"
	if [ ! -f "$file" ]; then
		mkdir -p "$WORK/secrets" && chmod 700 "$WORK/secrets"
		openssl rand -base64 "$2" >"$file"
		chmod 600 "$file"
	fi
	cat "$file"
}

# stop_forwards: the run's port-forwards end with it
stop_forwards() {
	[ -f "$WORK/forwards.pid" ] || return 0
	local p
	while read -r p; do kill "$p" 2>/dev/null || true; done <"$WORK/forwards.pid"
	rm -f "$WORK/forwards.pid"
}

# forward <namespace> <target> <port>: a port-forward of the run's; prints the local port
forward() {
	local log port=""
	log="$WORK/forward.$1.$(echo "$2" | tr '/' '_').log"
	k -n "$1" port-forward "$2" ":$3" >"$log" 2>&1 &
	# called in $(...) too: the pid goes to a file the run's exit reads, a subshell's array would lose it
	echo "$!" >>"$WORK/forwards.pid"
	for _ in $(seq 60); do
		port="$(sed -n 's/^Forwarding from 127.0.0.1:\([0-9]*\) .*/\1/p' "$log" | head -1)"
		[ -n "$port" ] && break
		sleep 0.5
	done
	[ -n "$port" ] || fail "no port-forward to $1/$2"
	echo "$port"
}
