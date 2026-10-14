# Build with the context produced by docker-context.py; binaries are not rebuilt here.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
ARG TARGETARCH
COPY --chown=65532:65532 bin/linux/${TARGETARCH}/ /usr/local/bin/
COPY --chown=65532:65532 data/ /data/
COPY provenance.json /usr/local/share/oss-transfer/provenance.json
ENV PATH=/usr/local/bin:/usr/bin:/bin
WORKDIR /data
USER 65532:65532
VOLUME ["/data"]
EXPOSE 1685
STOPSIGNAL SIGTERM
CMD ["oss-transfer-master", "--listen", ":1685", "--data-dir", "/data/master"]
