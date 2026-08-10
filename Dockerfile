FROM container-registry.oracle.com/os/oraclelinux:9-slim
RUN mkdir /app && microdnf update -y && microdnf clean all
COPY --chmod=755 build/obsgateway-linux-amd64 /usr/bin/
ENTRYPOINT [ "/usr/bin/obsgateway-linux-amd64" ]