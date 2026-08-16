# Multi-stage: the final image carries a static binary and nothing else -- no
# shell, no package manager, no Go toolchain. Less to patch, and nothing for a
# compromised process to pivot into.

FROM golang:1.25-alpine AS build
WORKDIR /src

# Copy manifests first so dependency download is cached independently of source
# changes; go.sum pins every module by hash.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO off keeps the binary static so it runs on a distroless base. The version
# stamp makes a running container traceable to a commit.
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/freightalerts ./cmd/freightalerts

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/freightalerts /freightalerts

# Runs unprivileged. The distroless nonroot tag provides uid 65532 with no
# shell, so an RCE has no interpreter to reach for.
USER nonroot:nonroot
EXPOSE 8080

# serve is the default; `poll` and `migrate` are passed as the command instead.
ENTRYPOINT ["/freightalerts"]
CMD ["serve"]
