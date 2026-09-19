# truegrain, as a container.
#
# The image serves the engine against BigQuery. It deliberately does not carry
# the DuckDB CLI: the DuckDB executor shells out to it, and bundling a binary
# fetched at build time means either trusting an unverified download or pinning
# a checksum that goes stale. DuckDB is the local quickstart, which is what
# `make demo` is for; a container is what you run against a real warehouse.
#
# Credentials are never baked in. BigQuery authenticates with Application
# Default Credentials, which on Cloud Run, GKE and Compute Engine come from the
# metadata server, and locally from a mounted gcloud configuration.

# The console is built first, into the directory the Go embed directive
# reads. One image carries both halves, so the UI and the API it talks to
# cannot be different versions: a console served from a CDN against an
# engine deployed last week is a bug with no symptom except a screen that
# renders half its data.
FROM node:24-alpine AS console

WORKDIR /console

# Manifests first, so a source change does not refetch the dependency
# tree.
COPY console/package.json console/package-lock.json ./
RUN npm ci

COPY console/ ./
# The output directory is named explicitly. Vite's default is a path
# relative to the repository root, which does not exist in this stage.
RUN CONSOLE_OUT=/out-dist npm run build

FROM golang:1.25-alpine AS build

WORKDIR /src

# Dependencies first, so a source change does not refetch the module graph.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# The built console replaces the committed placeholder. Without this the
# image would serve the page that says the console is not built, which is
# the correct message and the wrong one for a release artifact.
COPY --from=console /out-dist/ ./internal/console/dist/

# Version metadata is passed in rather than guessed, so the binary can say
# which commit produced it. A release artifact that cannot be traced back to a
# commit is an artifact nobody can audit.
ARG VERSION=dev
ARG COMMIT=""
ARG BUILD_DATE=""

# CGO_ENABLED=0 produces a static binary, which is what lets the runtime stage
# be distroless: no libc, no shell, nothing to exec into.
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w \
        -X github.com/rk-chavali/truegrain/internal/version.version=${VERSION} \
        -X github.com/rk-chavali/truegrain/internal/version.commit=${COMMIT} \
        -X github.com/rk-chavali/truegrain/internal/version.date=${BUILD_DATE}" \
      -o /out/truegrain ./cmd/truegrain

# Prove the binary runs before it is shipped. A broken artifact discovered by a
# user is a worse outcome than a failed build.
RUN /out/truegrain version

FROM gcr.io/distroless/static-debian12:nonroot

# Distroless carries the CA bundle already, which BigQuery and any OIDC issuer
# need for TLS. Nothing else is present: no shell, no package manager.
COPY --from=build /out/truegrain /usr/local/bin/truegrain

# Runs as uid 65532. Nothing in the engine needs root, and a semantic layer
# holding warehouse credentials is exactly the kind of process that should not
# have it.
USER nonroot:nonroot

# A model repository is mounted here. It is read only as far as the engine is
# concerned: nothing in this image writes to a model.
WORKDIR /models

EXPOSE 8080

ENTRYPOINT ["truegrain"]
# Bound to all interfaces because a container's loopback is only its own. The
# engine refuses to serve a non-loopback address without authentication, so
# this default cannot quietly expose an unauthenticated deployment.
CMD ["serve", "rest", "-models", "/models", "-addr", "0.0.0.0:8080"]

# `serve console` is the other way to run this image: the control plane,
# the API and the UI on one port. It needs TRUEGRAIN_CONTROL_DSN and
# TRUEGRAIN_CONTROL_KEY, and deploy/compose/compose.yaml shows both.
