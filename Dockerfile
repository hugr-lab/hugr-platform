# the operator's image (spec 001), the front's too (spec 002), the console's (spec 003), the install Job's (spec 005):
# static binaries on distroless, non-root. linux/amd64 and linux/arm64: Go cross-compiles on the build machine and the
# final stage only copies, so no stage runs under emulation (spec 005's release)

# the web console's build: the licence gate, then the build the operator embeds
FROM --platform=$BUILDPLATFORM node:22-alpine AS console
WORKDIR /web
COPY web/console/package.json web/console/package-lock.json ./
RUN npm ci --no-fund --no-audit --ignore-scripts
COPY web/console/ ./
RUN npm run licenses && npm run build && test -f dist/index.html

FROM --platform=$BUILDPLATFORM golang:1.27.3 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=console /web/dist ./web/console/dist
ENV CGO_ENABLED=0 GOWORK=off GOOS=$TARGETOS GOARCH=$TARGETARCH
RUN go build -trimpath -ldflags "-s -w" -o /operator ./cmd/operator && \
    go build -trimpath -ldflags "-s -w" -o /front ./cmd/front && \
    go build -trimpath -ldflags "-s -w" -o /hugr-init ./cmd/hugr-init

FROM gcr.io/distroless/static:nonroot
COPY --from=build /operator /operator
# the front (spec 002): one image, two binaries - the operator starts it with this image
COPY --from=build /front /front
# the install Job (spec 005): the chart runs it with this image
COPY --from=build /hugr-init /hugr-init
USER 65532:65532
ENTRYPOINT ["/operator"]
