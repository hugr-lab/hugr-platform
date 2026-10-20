# shellcheck shell=bash
# The console for a person's browser (spec 003): `run.sh <kind | minikube> console` after an up - Keycloak and the
# console forwarded to this machine on the ports the realm's console client names, until Ctrl-C. The browser reaches
# Keycloak by the issuer's own name (the nodes trust https://keycloak.idp.svc:8443/realms/hugr), so that name resolves
# to 127.0.0.1 here: a line of /etc/hosts, the person's to add. The stand's forwards are its own (stand.pid): a check
# or an up run beside it neither ends them nor is ended by them.
STAND_KC_PORT=8443
STAND_CONSOLE_PORT=8444

# stop_stand: the stand's forwards (only port-forwards: a stale pid may be somebody else's by now)
stop_stand() {
	[ -f "$WORK/stand.pid" ] || return 0
	local p
	while read -r p; do
		ps -p "$p" -o command= 2>/dev/null | grep -q 'kubectl.*port-forward' && kill "$p" 2>/dev/null || true
	done <"$WORK/stand.pid"
	rm -f "$WORK/stand.pid"
}

# stand_forward <namespace> <target> <local port> <port> <name>: in a subshell, so no job of this shell (no notice
# when it ends); its pid in stand.pid, its log where diagnose reads the run's forwards
stand_forward() {
	(command kubectl --context "$DEMO_CONTEXT" -n "$1" port-forward "$2" "$3:$4" </dev/null \
		>"$WORK/forward.stand-$5.log" 2>&1 &
	echo "$!" >>"$WORK/stand.pid")
}

console_stand() {
	[ "$IDP_NAME" = Keycloak ] || fail "the console stand signs a person in at the demo's Keycloak: kind or minikube"
	k -n hugr get deploy hugr-platform >/dev/null 2>&1 || fail "no platform here: run up first"
	[ -s "$WORK/secrets/person-operator" ] || fail "no console person in this run's realm: run up again"
	step "the console for a browser (spec 003)"
	provider_metrics
	stop_stand # a stand killed before its exit left its forwards
	trap 'stop_stand; stop_forwards' EXIT
	local port
	for port in $STAND_KC_PORT $STAND_CONSOLE_PORT; do
		! (echo >"/dev/tcp/127.0.0.1/$port") 2>/dev/null || fail "port $port is taken on this machine"
	done
	stand_forward idp svc/keycloak "$STAND_KC_PORT" 8443 keycloak
	stand_forward hugr svc/hugr-platform-console "$STAND_CONSOLE_PORT" 443 console
	eventually 30 "Keycloak answers on this machine" curl -fsS --cacert "$WORK/ca.crt" \
		--resolve "keycloak.idp.svc:$STAND_KC_PORT:127.0.0.1" \
		"https://keycloak.idp.svc:$STAND_KC_PORT/realms/hugr/.well-known/openid-configuration"
	eventually 30 "the console answers on this machine" curl -fsS --cacert "$WORK/ca.crt" \
		"https://127.0.0.1:$STAND_CONSOLE_PORT/api/v1/console/config"
	ok "the console: https://127.0.0.1:$STAND_CONSOLE_PORT"
	if ! grep -Eq '^[[:space:]]*127\.0\.0\.1([[:space:]]+[^#[:space:]]+)*[[:space:]]+keycloak\.idp\.svc([[:space:]]|$)' /etc/hosts; then
		echo "  the browser reaches Keycloak as keycloak.idp.svc: add to /etc/hosts (yours to add, once)"
		echo "    127.0.0.1 keycloak.idp.svc"
	fi
	echo "  sign in as operator, the password in $WORK/secrets/person-operator"
	echo "  the run's CA (trust it, or accept the browser's warning for both addresses): $WORK/ca.crt"
	echo "  open https://keycloak.idp.svc:$STAND_KC_PORT/ first if the CA is not trusted; Ctrl-C ends the stand"
	# until Ctrl-C, or until a forward ends (a pod it reached rolled: Keycloak by a new realm, the operator)
	local p
	while true; do
		while read -r p; do
			kill -0 "$p" 2>/dev/null || fail "a forward of the stand ended (its pod rolled?): run console again"
		done <"$WORK/stand.pid"
		sleep 2
	done
}
