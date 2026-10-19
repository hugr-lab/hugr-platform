# the operator's image (spec 001), the front's too (spec 002): static binaries on distroless, non-root
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOWORK=off go build -trimpath -ldflags "-s -w" -o /operator ./cmd/operator && \
    CGO_ENABLED=0 GOWORK=off go build -trimpath -ldflags "-s -w" -o /front ./cmd/front

FROM gcr.io/distroless/static:nonroot
COPY --from=build /operator /operator
# the front (spec 002): one image, two binaries - the operator starts it with this image
COPY --from=build /front /front
USER 65532:65532
ENTRYPOINT ["/operator"]
