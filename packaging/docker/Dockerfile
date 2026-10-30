# BackupZit console as a container image. It is built from the signed
# server package of a release (the same files as the appliance and the
# Linux packages), including the agent installers the console offers:
#
#   docker build --build-arg VERSION=0.46.2 -t backupzit:0.46.2 .
#
# with backupzit-server_<VERSION>_amd64.deb in this folder. The image holds
# only the console (a static program), CA certificates and time zones — no
# shell or system tools — and runs as an unprivileged user.
FROM debian:bookworm-slim AS package
ARG VERSION
COPY backupzit-server_${VERSION}_amd64.deb /tmp/server.deb
RUN dpkg-deb -x /tmp/server.deb /pkg \
 && install -d -m 0700 -o 65532 -g 65532 /pkg/var/lib/backupzit

FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION
LABEL org.opencontainers.image.title="BackupZit" \
      org.opencontainers.image.description="BackupZit console: self-hosted backup for Windows, Linux, Proxmox, VMware, Hyper-V, databases, NAS and Microsoft 365" \
      org.opencontainers.image.source="https://github.com/Max-Zit/backupzit" \
      org.opencontainers.image.url="https://backupzit.com/" \
      org.opencontainers.image.vendor="MaxZit" \
      org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=package /pkg/usr/bin/backupzit-server /usr/bin/backupzit-server
COPY --from=package /pkg/usr/share/backupzit/dist /usr/share/backupzit/dist
COPY --from=package /pkg/usr/share/doc/backupzit-server /usr/share/doc/backupzit-server
COPY --from=package --chown=65532:65532 /pkg/var/lib/backupzit /var/lib/backupzit
ENV BACKUPZIT_CONTAINER=docker \
    BACKUPZIT_DATA_DIR=/var/lib/backupzit \
    BACKUPZIT_LISTEN=:8443
USER 65532:65532
WORKDIR /var/lib/backupzit
VOLUME /var/lib/backupzit
EXPOSE 8443
HEALTHCHECK --interval=30s --timeout=10s --start-period=60s \
  CMD ["/usr/bin/backupzit-server", "--healthcheck"]
ENTRYPOINT ["/usr/bin/backupzit-server"]
