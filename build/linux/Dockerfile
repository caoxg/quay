# Build environment for the Linux GUI: Go + GTK 3 + WebKitGTK 4.1 (Debian 12, glibc 2.36).
# Used by `make linux`; build it per platform (linux/amd64, linux/arm64).
FROM golang:1.26-bookworm
RUN apt-get update \
 && apt-get install -y --no-install-recommends libgtk-3-dev libwebkit2gtk-4.1-dev \
 && rm -rf /var/lib/apt/lists/*
ENV CGO_ENABLED=1 GOFLAGS=-buildvcs=false
WORKDIR /src
