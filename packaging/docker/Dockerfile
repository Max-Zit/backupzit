# BackupZit console as a container image. It is built from the signed
# server package of a release (the same files as the appliance and the
# Linux packages), including the agent installers the console offers:
#
#   docker build --build-arg VERSION=0.46.0 -t backupzit:0.46.0 .
#
# with backupzit-server_<VERSION>_amd64.deb in this folder.
FROM debian:bookworm-slim
ARG VERSION
LABEL org.opencontainers.image.title="BackupZit" \
      org.opencontainers.image.description="BackupZit console: self-hosted backup for Windows, Linux, Proxmox, VMware, Hyper-V, databases, NAS and Microsoft 365" \
      org.opencontainers.image.source="https://github.com/Max-Zit/backupzit" \
      org.opencontainers.image.url="https://backupzit.com/" \
      org.opencontainers.image.vendor="MaxZit" \
      org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.version="${VERSION}"
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tzdata \
 && rm -rf /var/lib/apt/lists/*
COPY backupzit-server_${VERSION}_amd64.deb /tmp/server.deb
RUN dpkg-deb -x /tmp/server.deb /tmp/pkg \
 && install -m 0755 /tmp/pkg/usr/bin/backupzit-server /usr/bin/backupzit-server \
 && mkdir -p /usr/share/backupzit /usr/share/doc \
 && cp -r /tmp/pkg/usr/share/backupzit/dist /usr/share/backupzit/dist \
 && cp -r /tmp/pkg/usr/share/doc/backupzit-server /usr/share/doc/backupzit-server \
 && rm -rf /tmp/pkg /tmp/server.deb \
 && useradd --system --uid 10001 --user-group --home-dir /var/lib/backupzit --shell /usr/sbin/nologin backupzit \
 && install -d -m 0700 -o backupzit -g backupzit /var/lib/backupzit
COPY entrypoint.sh /usr/local/bin/backupzit-entrypoint
ENV BACKUPZIT_CONTAINER=docker \
    BACKUPZIT_DATA_DIR=/var/lib/backupzit \
    BACKUPZIT_LISTEN=:8443
USER backupzit
WORKDIR /var/lib/backupzit
VOLUME /var/lib/backupzit
EXPOSE 8443
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s \
  CMD ["bash", "-c", "exec 3<>/dev/tcp/127.0.0.1/8443"]
ENTRYPOINT ["/usr/local/bin/backupzit-entrypoint"]
