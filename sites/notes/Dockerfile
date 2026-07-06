# syntax=docker/dockerfile:1
# QinCloud Learnings — Quartz static site of the learnings/ vault.
#
# Multi-stage: Node builds the vault into static HTML, nginx serves it. Quartz
# is pinned to a specific commit (not a moving branch) so a rebuild months from
# now produces the same site — the-box-is-disposable, applied to this image.
#
# Build from the REPO ROOT (context needs both learnings/ and sites/notes/):
#   docker build -f sites/notes/Dockerfile -t ghcr.io/muttyqt15/qcloud-notes:v1 .
FROM node:22-bookworm-slim AS build
WORKDIR /quartz
RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates \
 && rm -rf /var/lib/apt/lists/*

# Pinned Quartz v4 commit (2026-04-20). Bump deliberately, not implicitly.
ARG QUARTZ_REF=d25a6eabf96751ffca56f8a8139272def7a65041
RUN git clone https://github.com/jackyzha0/quartz.git . \
 && git checkout "${QUARTZ_REF}"
RUN npm ci

# Our config replaces Quartz's default; the vault replaces Quartz's sample content.
COPY sites/notes/quartz.config.ts ./quartz.config.ts
RUN rm -rf content && mkdir content
COPY learnings/ ./content/
# The vault's entry note (README = the Map of Content) becomes the homepage.
# Its frontmatter slug is "readme"; the root page must resolve at "/".
RUN mv content/README.md content/index.md \
 && sed -i 's/^slug:.*/slug: index/' content/index.md

RUN npx quartz build

FROM nginx:1.27-alpine
COPY sites/notes/nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /quartz/public /usr/share/nginx/html
EXPOSE 80
