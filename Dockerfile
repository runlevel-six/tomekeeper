# syntax=docker/dockerfile:1

# Build stage. Pinned to a minor series rather than a patch so security patches
# arrive without a commit, and rebuilt from scratch in CI.
FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies first, so the module cache layer survives source edits.
# go.sum joins this line when the first dependency lands (M1).
COPY go.mod ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/

# Build identity is injected rather than derived: the .git directory is not in
# the build context (see .dockerignore), so -buildvcs would have nothing to
# read and would fail the build rather than silently omitting the stamp.
ARG VERSION=dev
ARG COMMIT=""
ARG DATE=""

ENV CGO_ENABLED=0

RUN go build \
      -trimpath \
      -buildvcs=false \
      -ldflags "-s -w \
        -X github.com/runlevel-six/tomekeeper/internal/version.Version=${VERSION} \
        -X github.com/runlevel-six/tomekeeper/internal/version.Commit=${COMMIT} \
        -X github.com/runlevel-six/tomekeeper/internal/version.Date=${DATE}" \
      -o /out/tome ./cmd/tome

# Runtime stage. distroless/static has no shell, no package manager, and no
# libc — there is nothing in the image to exploit but the binary itself. This
# is only possible because the build is CGO-free and fully static.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/tome /usr/local/bin/tome

USER nonroot:nonroot
EXPOSE 8080

# No HEALTHCHECK: there is no shell or curl in the image to run one with, and
# the orchestrator probes /healthz and /readyz directly. See
# docs/reference/cli.md.
ENTRYPOINT ["/usr/local/bin/tome"]
CMD ["serve"]
