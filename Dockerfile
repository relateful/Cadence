FROM golang:1.26-bookworm AS build
ARG LIBDAVE_VERSION=v1.1.0
ARG GODAVE_VERSION=v0.3.0
RUN apt-get update && apt-get install -y --no-install-recommends libopus-dev pkg-config curl unzip ca-certificates && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL -o /tmp/libdave_install.sh https://raw.githubusercontent.com/disgoorg/godave/${GODAVE_VERSION}/scripts/libdave_install.sh \
 && NON_INTERACTIVE=1 sh /tmp/libdave_install.sh ${LIBDAVE_VERSION}
ENV PKG_CONFIG_PATH=/root/.local/lib/pkgconfig
ENV CGO_ENABLED=1
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go test ./... && go build -tags native,nolibopusfile -trimpath -ldflags "-s -w" -o /out/cadence ./cmd/cadence

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends libopus0 ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=build /root/.local/lib/libdave.so /usr/local/lib/libdave.so
RUN ldconfig
COPY --from=build /out/cadence /usr/local/bin/cadence
USER nobody
EXPOSE 2333
ENTRYPOINT ["/usr/local/bin/cadence"]
