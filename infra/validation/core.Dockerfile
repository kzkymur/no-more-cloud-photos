FROM golang:1.27.1-bookworm@sha256:69a7b9788769bec032d238959b61854e9ae87f57be9029ec04e9885fabf99195 AS build

ENV GOTOOLCHAIN=local
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/core-api ./cmd/core-api \
 && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/core-worker ./cmd/core-worker \
 && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/nmcp-admin ./cmd/nmcp-admin

FROM ubuntu:24.04@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl ffmpeg libimage-exiftool-perl util-linux \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd --gid 10001 nmcp \
 && useradd --uid 10001 --gid 10001 --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin nmcp
COPY --from=build /out/core-api /out/core-worker /out/nmcp-admin /usr/local/bin/
USER 10001:10001
WORKDIR /nonexistent
