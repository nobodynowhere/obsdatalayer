FROM --platform=linux/amd64 container-registry.oracle.com/os/oraclelinux@sha256:a98d379f4c1a48d86bebc974982a3ba1a71441ffc7f39b560d9e8ffe0f360d06

RUN mkdir -p /etc/obsgateway /var/lib/obsgateway \
    && chown -R 65532:65532 /etc/obsgateway /var/lib/obsgateway \
    && microdnf clean all

COPY --chmod=755 build/obsgateway-linux-amd64 /usr/bin/
COPY --chown=65532:65532 src/obsgateway.yml /etc/obsgateway/obsgateway.yml

USER 65532:65532
EXPOSE 8443 9991 4317 4318
ENTRYPOINT [ "/usr/bin/obsgateway-linux-amd64" ]
CMD [ "-config", "/etc/obsgateway/obsgateway.yml" ]
