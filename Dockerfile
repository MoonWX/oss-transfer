# Build the release first: ./build.sh
ARG JAVA_IMAGE=eclipse-temurin:8-jre-jammy
FROM ${JAVA_IMAGE}
ARG TARGETARCH
ARG PACKAGE_DIR=output/oss-transfer

RUN apt-get update && apt-get install -y --no-install-recommends curl openssl \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 transfer \
    && useradd --uid 10001 --gid transfer --home-dir /app/data --no-create-home transfer \
    && mkdir -p /opt/oss-transfer /app/data /data \
    && chown -R 10001:10001 /app/data /data
COPY ${PACKAGE_DIR}/oss-transfer.jar /opt/oss-transfer/oss-transfer.jar
COPY ${PACKAGE_DIR}/agent-bundle.tar.gz /opt/oss-transfer/agent-bundle.tar.gz
COPY ${PACKAGE_DIR}/oss-transfer-cli-${TARGETARCH} /opt/oss-transfer/oss-transfer-cli
COPY deploy/docker/docker-defaults.yml /opt/oss-transfer/docker-defaults.yml
COPY deploy/docker/entrypoint.sh deploy/docker/healthcheck.sh /opt/oss-transfer/
COPY deploy/docker/cli.sh /usr/local/bin/oss-transfer-cli
RUN chmod 0755 /opt/oss-transfer/oss-transfer-cli /opt/oss-transfer/entrypoint.sh \
    /opt/oss-transfer/healthcheck.sh /usr/local/bin/oss-transfer-cli
ENV HOME=/app/data \
    TRANSFER_ROLE=master \
    OSS_TRANSFER_PORT=1685 \
    JAVA_OPTS="-Xms128m -Xmx1024m"
WORKDIR /app/data
USER 10001:10001
VOLUME ["/app/data"]
EXPOSE 1685
STOPSIGNAL SIGTERM
HEALTHCHECK --interval=15s --timeout=6s --start-period=90s --retries=4 \
    CMD ["/opt/oss-transfer/healthcheck.sh"]
ENTRYPOINT ["/opt/oss-transfer/entrypoint.sh"]
