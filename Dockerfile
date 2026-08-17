# syntax=docker/dockerfile:1

# Build stage. Pinned to a minor series rather than a patch so security patches
# arrive without a commit, and rebuilt from scratch in CI.
FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies first, so the module cache layer survives source edits.
COPY go.mod go.sum ./
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

# -tags nodynamic is what makes the runtime stage below possible, and it is not
# optional. The AVIF encoder pulls in ebitengine/purego, which uses
# //go:cgo_import_dynamic to reach dlopen — so the binary links against
# libc.so.6 and needs an ELF interpreter *even with CGO_ENABLED=0*. In an image
# with no libc that produces "exec /usr/local/bin/tome: no such file or
# directory", which is a confusing way to say the loader is missing.
#
# The tag drops purego's dlopen path, which only existed to use a system libavif
# when one happens to be installed. Encoding still runs through the bundled
# WebAssembly build, which is the path this project intended all along.
#
# scripts/smoke.sh is what catches this if it ever regresses: it is the only test
# that runs the binary in an image without a libc to fall back on.
RUN go build \
      -trimpath \
      -buildvcs=false \
      -tags nodynamic \
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
