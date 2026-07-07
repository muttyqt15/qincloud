# controld — static build, minimal runtime. Built on the box by compose
# (stack/controld/compose.yml → build: ../../controld); no registry needed.
# Bases are patch-pinned like every other image in this repo: a box rebuilt
# months later (invariant #4) must build against the same bases that were
# tested, not whatever the floating tag points at by then.
FROM golang:1.26.4-alpine3.22 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/controld ./cmd/controld

# alpine (not scratch): busybox wget for the compose healthcheck, plus git +
# ca-certificates for the notes web editor — it clones/pulls/pushes the mirror
# over https and builds the notes image on the daemon (see internal/authoring).
FROM alpine:3.22.5
RUN apk add --no-cache git ca-certificates
COPY --from=build /out/controld /usr/local/bin/controld
# Deliberately root, no USER: the container mounts docker.sock (host-root-
# equivalent regardless of in-container uid) and Caddy's root-owned admin
# socket — dropping uid would break both mounts and buy zero real isolation.
# Containment comes from what can reach controld, not from its uid.
ENTRYPOINT ["controld"]
CMD ["serve"]
