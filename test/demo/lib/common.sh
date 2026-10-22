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
# what reaches the terminal (or CI's log) is blanked: every value of the run's secrets, a JWT, a bearer header
redact() {
	python3 -c 'import os, re, sys
d = sys.argv[1]
values = []
if os.path.isdir(d):
    for name in os.listdir(d):
        v = open(os.path.join(d, name), errors="replace").read().strip()
        if len(v) >= 8:
            values.append((v, name))
text = sys.stdin.buffer.read().decode(errors="replace")
for v, name in sorted(values, key=lambda x: -len(x[0])):
    text = text.replace(v, "<secret " + name + ">")
text = re.sub(r"eyJ[A-Za-z0-9._-]*", "<token>", text)
text = re.sub(r"(Bearer )[A-Za-z0-9._~+/=-]+", r"\1<token>", text)
sys.stdout.write(text)' "$WORK/secrets"
}

need() {
	local tool
	for tool in "$@"; do
		command -v "$tool" >/dev/null || {
			[ -n "${CI:-}" ] && fail "$tool not found" || skip "$tool not found"
		}
	done
}

# bounded <seconds> <command...>: an external command that may not hang the run - ended and failed past its time
# where timeout(1) is at hand (Linux, CI; GNU coreutils' gtimeout on macOS), run as it is otherwise
bounded() {
	local secs="$1"
	shift
	if command -v timeout >/dev/null; then
		timeout "$secs" "$@"
	elif command -v gtimeout >/dev/null; then
		gtimeout "$secs" "$@"
	else
		"$@"
	fi
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

# diagnose: the cluster's state - pods, events, the platform's objects and logs - every value of the run's secrets and
# every token blanked
diagnose() {
	{
		k get pods -A -o wide
		k get events -A --sort-by=.lastTimestamp | tail -80
		k -n hugr get hng,hp -o yaml
		k -n hugr logs deploy/hugr-platform --tail 100
		k -n hugr logs -l app.kubernetes.io/component=init --tail 40 --prefix
		local ns
		for ns in "$TRESOR_NS" "$KISTA_NS"; do
			echo "== $ns"
			k -n "$ns" logs -l app.kubernetes.io/name --tail 40 --prefix
		done
		echo "== openbao"
		k -n openbao logs -l app=openbao --tail 40 --prefix
		local pod
		for pod in $(k -n hugr get pods -l platform.hugr.io/group -o jsonpath='{.items[*].metadata.name}'); do
			echo "== $pod"
			k -n hugr logs "$pod" --tail 60
		done
		local log
		for log in "$WORK"/forward.*.log; do
			[ -f "$log" ] || continue
			echo "== ${log##*/}"
			tail -20 "$log"
		done
		echo "== the run's processes"
		ps -eo pid,ppid,etime,command | grep -E 'kubectl|check|duckdb|run.sh' | grep -v grep
	} 2>&1 | redact
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
# The CA lasts 30 days and is made again two days before it ends (its certificates with it); a v3 CA, whatever openssl
# makes one (macOS's LibreSSL writes a v1 certificate without basicConstraints otherwise)
ca_make() {
	[ -f "$WORK/ca.crt" ] && openssl x509 -checkend 172800 -noout -in "$WORK/ca.crt" >/dev/null 2>&1 && return 0
	rm -f "$WORK"/*.crt "$WORK"/*.key "$WORK"/*.csr "$WORK"/*.ext "$WORK"/*.srl
	(umask 077 && openssl req -x509 -newkey rsa:2048 -nodes -keyout "$WORK/ca.key" -out "$WORK/ca.crt" -days 30 \
		-subj "/CN=hugr-platform demo CA" -addext basicConstraints=critical,CA:TRUE \
		-addext keyUsage=critical,keyCertSign,cRLSign 2>/dev/null) || fail "the run's CA"
}

# ca_cert <name> <dns>...: a server certificate in $WORK/<name>.{crt,key}, also for 127.0.0.1 and localhost (a
# port-forward reaches it there)
# A certificate still valid for a day, of the CA as it is, for the names asked, is kept: the services serving it keep
# running on it (a service that moved namespace gets a new one).
ca_cert() {
	local name="$1" san="IP:127.0.0.1,DNS:localhost" dns kept=1
	shift
	for dns in "$@"; do san="$san,DNS:$dns"; done
	if [ -f "$WORK/$name.crt" ] && openssl x509 -checkend 86400 -noout -in "$WORK/$name.crt" >/dev/null 2>&1 &&
		openssl verify -CAfile "$WORK/ca.crt" "$WORK/$name.crt" >/dev/null 2>&1; then
		for dns in "$@"; do
			openssl x509 -noout -ext subjectAltName -in "$WORK/$name.crt" 2>/dev/null | grep -q "DNS:$dns\(,\|$\)" ||
				kept=""
		done
		[ -z "$kept" ] || return 0
	fi
	(umask 077 && openssl req -newkey rsa:2048 -nodes -keyout "$WORK/$name.key" -out "$WORK/$name.csr" -subj "/CN=$1" \
		2>/dev/null) || fail "the key of $name"
	printf 'subjectAltName=%s\nextendedKeyUsage=serverAuth\nbasicConstraints=CA:FALSE\n' "$san" >"$WORK/$name.ext"
	openssl x509 -req -in "$WORK/$name.csr" -CA "$WORK/ca.crt" -CAkey "$WORK/ca.key" -CAcreateserial -days 29 \
		-extfile "$WORK/$name.ext" -out "$WORK/$name.crt" 2>/dev/null || fail "the certificate of $name"
}

# tls_secret <namespace> <name> <cert name>: a kubernetes.io/tls Secret of the certificate; prints "changed" when it
# was another one (the service serving it is restarted by its caller)
tls_secret() {
	local now
	now="$(openssl base64 -A <"$WORK/$3.crt")"
	[ "$(k -n "$1" get secret "$2" -o jsonpath='{.data.tls\.crt}' 2>/dev/null)" = "$now" ] && return 0
	k -n "$1" delete secret "$2" --ignore-not-found >/dev/null
	k -n "$1" create secret tls "$2" --cert "$WORK/$3.crt" --key "$WORK/$3.key" >/dev/null || fail "the Secret $1/$2"
	echo changed
}

# namespace <name>: made, once a deletion of it (a down just before) has finished
namespace() {
	local phase
	phase="$(k get namespace "$1" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
	if [ "$phase" = Terminating ]; then
		k wait --for=delete "namespace/$1" --timeout=300s >/dev/null 2>&1 || fail "namespace $1 is still being deleted"
		phase=""
	fi
	[ -n "$phase" ] || k create namespace "$1" >/dev/null || fail "namespace $1"
}

# secret_file <name> <command...>: the file of a value of the run's, made once by the command and kept in its state
# (mode 0600, no newline: a Secret is made --from-file, never with the value on a command line); prints the path
secret_file() {
	local file="$WORK/secrets/$1"
	shift
	mkdir -p "$WORK/secrets" && chmod 700 "$WORK/secrets"
	if [ ! -s "$file" ]; then
		(umask 077 && "$@" | tr -d '\n' >"$file")
	elif [ -n "$(tail -c1 "$file" | tr -d '\n')" ]; then
		: # already without a newline
	else
		(umask 077 && tr -d '\n' <"$file" >"$file.new" && mv "$file.new" "$file")
	fi
	mask "$file"
	echo "$file"
}
# mask <file>: in CI, the value is masked in the job's log from here on (the runner reads its commands on stderr too)
mask() { [ "${GITHUB_ACTIONS:-}" != true ] || echo "::add-mask::$(cat "$1")" >&2; }
# secret_value <name>: a value of the run's, never printed
secret_value() { cat "$(secret_file "$1" openssl rand -hex 24)"; }
# secret_b64 <name> <bytes>: a key of the run's, base64
secret_b64() { cat "$(secret_file "$1" openssl rand -base64 "$2")"; }

# stop_forwards: the run's port-forwards and log followers end with it
stop_forwards() {
	[ -f "$WORK/forwards.pid" ] || return 0
	local p
	# a pid of a run killed before its exit may be somebody else's by now: only a port-forward or a log follower ends
	while read -r p; do
		ps -p "$p" -o command= 2>/dev/null | grep -Eq 'kubectl.*(port-forward|logs -f)' && kill "$p" 2>/dev/null || true
	done <"$WORK/forwards.pid"
	rm -f "$WORK/forwards.pid"
}

# forward <namespace> <target> <port>: a port-forward of the run's; prints the local port
forward() {
	local log port=""
	log="$WORK/forward.$1.$(echo "$2" | tr '/' '_').log"
	# kubectl itself in the background, never the function k: bash 5 runs a backgrounded function in a subshell that
	# keeps the descriptors it inherited - a caller's $(...) pipe among them - for as long as the port-forward lives,
	# and the $(...) never ends (main's demo run 39170307122 waited in check 3 for an hour); $! is then kubectl's
	# own pid, which stop_forwards recognises. Called in $(...) too: the pid goes to a file the run's exit reads.
	command kubectl --context "$DEMO_CONTEXT" -n "$1" port-forward "$2" ":$3" </dev/null >"$log" 2>&1 &
	echo "$!" >>"$WORK/forwards.pid"
	for _ in $(seq 60); do
		port="$(sed -n 's/^Forwarding from 127.0.0.1:\([0-9]*\) .*/\1/p' "$log" | head -1)"
		[ -n "$port" ] && break
		sleep 0.5
	done
	[ -n "$port" ] || fail "no port-forward to $1/$2"
	echo "$port"
}
