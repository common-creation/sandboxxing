# The MicroVM image of the sandboxxing microvm backend. Lambda builds it on
# top of its Amazon Linux 2023 base image (ARM64). The agent binary is placed
# next to this file by `make agent`.
FROM public.ecr.aws/lambda/microvms:al2023-minimal

# The agent runs as PID 1 of the container; tini reaps the orphaned processes
# that a login leaves behind.
ARG TINI_VERSION=v0.19.0
ADD https://github.com/krallin/tini/releases/download/${TINI_VERSION}/tini-static-arm64 /usr/local/bin/tini

# amazon-efs-utils mounts the home directory from its access point, with TLS
# and IAM authorisation. openssh-server provides sftp-server for sftp and scp.
RUN chmod 0755 /usr/local/bin/tini \
    && if command -v dnf >/dev/null; then PM=dnf; else PM=microdnf; fi \
    && $PM install -y \
        amazon-efs-utils bash ca-certificates findutils git gzip hostname \
        iproute less openssh-clients openssh-server procps-ng shadow-utils sudo \
        tar util-linux vim-minimal which \
    && $PM clean all \
    && mkdir -p /root && chmod 0700 /root

COPY sandboxxing-agent /usr/local/bin/sandboxxing-agent

ENV PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    HOME=/root \
    SHELL=/bin/bash

# 8080 is the data port that the MicroVM endpoint forwards to; the lifecycle
# hooks are served on 9000, which the endpoint does not expose.
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/tini", "--", "/usr/local/bin/sandboxxing-agent"]
