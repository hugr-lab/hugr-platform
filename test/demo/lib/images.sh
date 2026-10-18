# shellcheck shell=bash
# The demo's images (spec 004): the operator from this checkout; tresor-server and kista from a checkout each; hugr-node
# from the assets of a hugr-node release dry run (dist-linux_<arch>) - nothing new is published.

# src <repository>: a checkout of hugr-lab/<repository>'s main in the cache (or the directory the caller names)
src() {
	local repo="$1" var="$2" dir
	dir="${!var:-}"
	if [ -n "$dir" ]; then
		(cd "$dir" && pwd)
		return
	fi
	dir="$CACHE/src/$repo"
	if [ -d "$dir/.git" ]; then
		git -C "$dir" fetch -q --depth 1 origin main && git -C "$dir" reset -q --hard origin/main
	else
		mkdir -p "$CACHE/src"
		git clone -q --depth 1 "https://github.com/hugr-lab/$repo" "$dir"
	fi
	echo "$dir"
}

# node_dist: the node's assets for the cluster's architecture - HUGR_NODE_DIST, or the artifact of hugr-node's latest
# successful release dry run that has one
node_dist() {
	if [ -n "${HUGR_NODE_DIST:-}" ]; then
		(cd "$HUGR_NODE_DIST" && pwd)
		return
	fi
	local name="dist-linux_$ARCH" dir="$CACHE/$name" run
	run="$(gh run list -R hugr-lab/hugr-node --workflow release.yml --status success --limit 20 --json databaseId \
		--jq '.[].databaseId' | while read -r id; do
		gh api "repos/hugr-lab/hugr-node/actions/runs/$id/artifacts" --jq '.artifacts[] | select(.expired | not) | .name' |
			grep -qx "$name" && { echo "$id"; break; }
	done)"
	[ -n "$run" ] || fail "no hugr-node release run with $name (dispatch hugr-node's release workflow on main)"
	if [ ! -f "$dir/.run" ] || [ "$(cat "$dir/.run")" != "$run" ]; then
		rm -rf "$dir"
		gh run download "$run" -R hugr-lab/hugr-node -n "$name" -D "$dir" >/dev/null || fail "the artifact $name of run $run"
		echo "$run" >"$dir/.run"
	fi
	echo "$dir"
}

build_images() {
	step "the images"
	local dist tresor_src kista_src node_src
	(cd "$ROOT" && docker build -q -t hugr-platform:demo . >/dev/null) || fail "the operator's image"
	ok "the operator's image hugr-platform:demo, from this checkout"
	tresor_src="$(src tresor-server TRESOR_SERVER_DIR)"
	docker build -q -t tresor-server:demo --build-arg VERSION=demo "$tresor_src" >/dev/null || fail "tresor-server's image"
	kista_src="$(src duckdb-extension-repository KISTA_DIR)"
	docker build -q -t kista:demo "$kista_src" >/dev/null || fail "kista's image"
	ok "tresor-server:demo and kista:demo, from their main"
	NODE_SRC="$(src hugr-node HUGR_NODE_DIR)"
	if [ -n "${NODE_IMAGE:-}" ]; then
		docker image inspect "$NODE_IMAGE" >/dev/null 2>&1 || docker pull -q "$NODE_IMAGE" >/dev/null || fail "no $NODE_IMAGE"
		docker tag "$NODE_IMAGE" hugr-node:demo
		DIST=""
	else
		DIST="$(node_dist)"
		docker build -q -f "$NODE_SRC/docker/Dockerfile" --build-arg VERSION=demo -t hugr-node:demo "$DIST" >/dev/null ||
			fail "the node's image from $DIST"
	fi
	if [ -n "${NODE_IMAGE:-}" ]; then
		ok "hugr-node:demo, from $NODE_IMAGE"
	else
		ok "hugr-node:demo, from the release dry run assets in $DIST"
	fi
	for image in hugr-platform:demo tresor-server:demo kista:demo hugr-node:demo; do
		provider_load "$image"
	done
}
