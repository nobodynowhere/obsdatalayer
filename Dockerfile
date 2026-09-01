FROM container-registry.oracle.com/os/oraclelinux:9-slim
RUN mkdir -p /etc/obsgateway /var/lib/obsgateway && microdnf update -y && microdnf clean all
COPY --chmod=755 build/obsgateway-linux-amd64 /usr/bin/
COPY src/obsgateway.yml /etc/obsgateway/obsgateway.yml
EXPOSE 8443 9091 4317 4318
ENTRYPOINT [ "/usr/bin/obsgateway-linux-amd64" ]
CMD [ "-config", "/etc/obsgateway/obsgateway.yml" ]
