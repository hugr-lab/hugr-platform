# hugr-platform: the operator (spec 001). A workspace above the checkout (go.work) is not ours: GOWORK=off.
export GOWORK := off

CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.23.0
SETUP_ENVTEST ?= go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.25
STATICCHECK ?= go run honnef.co/go/tools/cmd/staticcheck@2026.1
# the API server and etcd of envtest, pinned (spec 001: the envtest binaries pinned)
ENVTEST_K8S_VERSION ?= 1.35.0
IMAGE ?= ghcr.io/hugr-lab/hugr-platform:dev

.PHONY: all generate manifests build test test-unit test-envtest fmt vet staticcheck lint image console console-test

all: generate manifests build test

## generate: the API types' deepcopy
generate:
	$(CONTROLLER_GEN) object paths=./api/...

## manifests: the CRDs, the operator's RBAC role and the webhook's registration, from the markers
manifests:
	$(CONTROLLER_GEN) crd rbac:roleName=hugr-platform-operator webhook paths=./... \
		output:crd:artifacts:config=config/crd output:rbac:artifacts:config=config/rbac \
		output:webhook:artifacts:config=config/webhook

build:
	go build ./...

fmt:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "gofmt: the files above" && exit 1)

vet:
	go vet ./...

staticcheck:
	$(STATICCHECK) ./...

lint: fmt vet staticcheck

## test: every test - the unit tests, and envtest against a real API server
test: test-unit test-envtest

test-unit:
	go test ./api/... ./pkg/... ./internal/...

test-envtest:
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" go test -tags envtest ./test/envtest/... -count=1

## console: the web console's build, embedded by web/console (spec 003): the licence gate first
console:
	cd web/console && npm ci --no-fund --no-audit && npm run licenses && npm run build

## console-test: its typecheck and tests
console-test:
	cd web/console && npm ci --no-fund --no-audit && npm run typecheck && npm test

image:
	docker build -t $(IMAGE) .
