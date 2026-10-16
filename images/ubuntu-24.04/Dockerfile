# SPDX-License-Identifier: MIT
#
# ermite-runner-ubuntu-24.04: the minimal CI-1 runner image. The same
# filesystem is used as an OCI image (container isolation) and exported to an
# ext4 base image (vm isolation) by scripts/build-image.sh. It holds the OS
# only: the runner executable is attached by the host at start (never baked
# in), so a runner upgrade does not need an image rebuild.
FROM ubuntu:24.04

ARG DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      bash build-essential ca-certificates curl e2fsprogs git gnupg iproute2 \
      iptables jq kmod procps python-is-python3 python3 ruby sudo tzdata unzip xz-utils zip \
 && rm -rf /var/lib/apt/lists/*

# The command-line tools of GitHub's ubuntu-24.04 image that workflows call
# directly (runner-images, images/ubuntu/toolsets/toolset-2404.json:
# apt.vital_packages and apt.cmd_packages), minus the daemons and services
# (haveged, pollinate, sphinxsearch, systemd-coredump). parallel pulls in
# sysstat, whose collection units are masked: a job image runs no services.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      acl aria2 binutils bison brotli bzip2 coreutils file findutils flex ftp \
      libnss3-tools lz4 m4 mediainfo netcat-openbsd net-tools p7zip-full parallel \
      patchelf pigz rsync shellcheck sqlite3 openssh-client sshpass swig telnet \
      time wget \
 && rm -rf /var/lib/apt/lists/* \
 && for u in sysstat.service sysstat-collect.timer sysstat-summary.timer; do ln -sf /dev/null /etc/systemd/system/$u; done

# Docker Engine from Docker's apt repository (the job gets its own daemon).
RUN install -m 0755 -d /etc/apt/keyrings \
 && curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc \
 && chmod a+r /etc/apt/keyrings/docker.asc \
 && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu noble stable" \
      > /etc/apt/sources.list.d/docker.list \
 && apt-get update \
 && apt-get install -y --no-install-recommends \
      docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin \
 && rm -rf /var/lib/apt/lists/*

# Node.js 22 and yarn 1 system-wide in /usr/local, like GitHub's image
# (runner-images, install-nodejs.sh:11-24 and toolset-2404.json: node.default
# 22, node_modules yarn). The tarball is checked against the SHA-256 published
# in https://nodejs.org/dist/v22.23.3/SHASUMS256.txt. /usr/local/lib/node_modules
# and /usr/local/bin are world-writable so that "npm install -g" works as the
# runner user (runner-images issue 3727).
ARG NODE_VERSION=22.23.3
ARG NODE_SHA256_AMD64=df450af89261115ef9f9e3830c3eeb2cc9213b63c720b1af623cb5dcbe2e02de
ARG NODE_SHA256_ARM64=a44aeb94849a299b22df10b9e622ec2f605c2183501bc40590705131de7c740f
RUN arch=$(dpkg --print-architecture) \
 && case "$arch" in \
      amd64) node_arch=x64; node_sha=$NODE_SHA256_AMD64 ;; \
      arm64) node_arch=arm64; node_sha=$NODE_SHA256_ARM64 ;; \
      *) echo "unsupported architecture $arch" >&2; exit 1 ;; \
    esac \
 && curl -fsSL "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${node_arch}.tar.xz" -o /tmp/node.tar.xz \
 && echo "${node_sha}  /tmp/node.tar.xz" | sha256sum -c - \
 && tar -xJf /tmp/node.tar.xz -C /usr/local --strip-components=1 --no-same-owner \
      --exclude='*/CHANGELOG.md' --exclude='*/LICENSE' --exclude='*/README.md' \
 && rm /tmp/node.tar.xz \
 && npm install -g yarn@1.22.22 \
 && npm cache clean --force \
 && chmod -R 777 /usr/local/lib/node_modules \
 && chmod 777 /usr/local/bin

# Like GitHub-hosted runners: user "runner" (uid 1001), passwordless sudo,
# member of "docker", with the job layout directories.
RUN useradd --create-home --uid 1001 --shell /bin/bash runner \
 && usermod -aG docker runner \
 && echo 'runner ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/runner \
 && chmod 0440 /etc/sudoers.d/runner \
 && mkdir -p /home/runner/work /opt/hostedtoolcache /opt/ermite \
 && chown -R runner:runner /home/runner /opt/hostedtoolcache

ENV RUNNER_TOOL_CACHE=/opt/hostedtoolcache \
    LANG=C.UTF-8

# The runner executable is bind-mounted (container) or provided by the init
# disk (vm) here; its init subcommand starts the job's own Docker daemon.
ENTRYPOINT ["/opt/ermite/ermite-runner", "init"]
