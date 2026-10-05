FROM golang:1.27.1-bookworm@sha256:69a7b9788769bec032d238959b61854e9ae87f57be9029ec04e9885fabf99195 AS build

ENV GOTOOLCHAIN=local
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/core-api ./cmd/core-api \
 && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/core-worker ./cmd/core-worker \
 && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/nmcp-admin ./cmd/nmcp-admin

FROM ubuntu:24.04@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55 AS still-build

ENV DEBIAN_FRONTEND=noninteractive
ENV PKG_CONFIG_PATH=/opt/nmcp/still-toolchain/lib/pkgconfig:/opt/nmcp/still-toolchain/lib64/pkgconfig
ENV LD_LIBRARY_PATH=/opt/nmcp/still-toolchain/lib:/opt/nmcp/still-toolchain/lib64
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      autoconf automake build-essential ca-certificates cmake curl git libtool \
      meson nasm ninja-build pkg-config xz-utils \
      libde265-dev libexif-dev libexpat1-dev libglib2.0-dev libjpeg-turbo8-dev libpng-dev \
      libwebp-dev zlib1g-dev \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY internal/stillprocessor/helper/ internal/stillprocessor/helper/
RUN internal/stillprocessor/helper/build-pinned-toolchain.sh \
      /opt/nmcp/still-toolchain /tmp/nmcp-still-toolchain-work \
 && cmake -S internal/stillprocessor/helper -B /tmp/nmcp-still-helper-build -G Ninja \
      -DCMAKE_BUILD_TYPE=Release -DBUILD_TESTING=OFF \
      -DCMAKE_PREFIX_PATH=/opt/nmcp/still-toolchain \
 && cmake --build /tmp/nmcp-still-helper-build --parallel 2 \
 && install -d /opt/nmcp/bin /opt/nmcp/share \
 && install -m 0755 /tmp/nmcp-still-helper-build/bin/nmcp-still-helper /opt/nmcp/bin/ \
 && curl --fail --location --silent --show-error \
      https://registry.color.org/rgb-registry/profiles/sRGB2014.icc \
      --output /opt/nmcp/share/sRGB2014.icc \
 && printf '%s  %s\n' \
      384b832de3412066743b52a75ee906b6fb9fb8d9e09e936fc2c43223815c6e0a \
      /opt/nmcp/share/sRGB2014.icc | sha256sum --check \
 && rm -rf /opt/nmcp/still-toolchain/include \
      /opt/nmcp/still-toolchain/lib/pkgconfig \
      /opt/nmcp/still-toolchain/lib64/pkgconfig \
      /opt/nmcp/still-toolchain/share

FROM ubuntu:24.04@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55

ENV DEBIAN_FRONTEND=noninteractive
ENV LD_LIBRARY_PATH=/opt/nmcp/still-toolchain/lib:/opt/nmcp/still-toolchain/lib64
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl ffmpeg libimage-exiftool-perl util-linux \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd --gid 10001 nmcp \
 && useradd --uid 10001 --gid 10001 --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin nmcp
COPY --from=build /out/core-api /out/core-worker /out/nmcp-admin /usr/local/bin/
COPY --from=still-build /opt/nmcp/ /opt/nmcp/
USER 10001:10001
WORKDIR /nonexistent
