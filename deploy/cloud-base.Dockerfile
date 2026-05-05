# weft cloud base image
#
# Pre-bakes the dependencies that DefaultOnStartCmd otherwise installs
# on every fresh rental. Eliminates the network steps (apt, astral.sh,
# rclone.org) that were the leading cause of bootstrap failures on
# Vast.ai — a transient hiccup on any one of those broke 6 of 7 recent
# launches and cost ~25 minutes per failure before weft's watchdog
# noticed.
#
# Build:
#   docker buildx build --platform linux/amd64 \
#     -t ghcr.io/osteele/weft-cloud-base:latest \
#     -f deploy/cloud-base.Dockerfile --push .
#
# After publishing, set [vastai] default_image in
# ~/.config/weft/config.toml to point here.

FROM docker.io/nvidia/cuda:12.4.1-runtime-ubuntu22.04

ENV DEBIAN_FRONTEND=noninteractive

# Single combined RUN so the layer commits in one shot — apt + curl-installs
# under qemu emulation are slow enough that splitting layers risks individual
# RPC timeouts during build. unzip is needed by the rclone installer; the
# rest are minimum runtime tools the bootstrap and agent expect.
RUN apt-get update -qq && \
    apt-get install -y -qq --no-install-recommends \
        ca-certificates curl unzip openssh-server python3-pip && \
    rm -rf /var/lib/apt/lists/* && \
    curl -fsSL https://rclone.org/install.sh | bash && \
    curl -fsSL https://astral.sh/uv/install.sh | sh && \
    install -m 0755 /root/.local/bin/uv /usr/local/bin/uv && \
    install -m 0755 /root/.local/bin/uvx /usr/local/bin/uvx && \
    pip3 install --no-cache-dir 'huggingface_hub[cli]'

# Marker so DefaultOnStartCmd can detect a pre-baked image and skip its
# install steps.
ENV WEFT_BASE_IMAGE=1
RUN echo 'weft cloud base image' > /etc/weft-base-image
