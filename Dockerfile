FROM golang:1.26-trixie AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ptb ./cmd/ptb


FROM debian:trixie-slim

# Mercado Livre blocks headless Chrome outright, so Chromium runs headful
# against an Xvfb display. chromium-sandbox is deliberately absent: we run
# --no-sandbox inside an already-isolated container.
RUN apt-get update && apt-get install -y --no-install-recommends \
      chromium \
      xvfb \
      tini \
      ca-certificates \
      fonts-liberation \
      fonts-noto-color-emoji \
      tzdata \
 && rm -rf /var/lib/apt/lists/*

ENV DISPLAY=:99 \
    DATA_DIR=/data \
    CHROME_PATH=/usr/bin/chromium \
    TZ=America/Sao_Paulo

# Xvfb cannot create its socket directory as a non-root user.
RUN mkdir -p /tmp/.X11-unix && chmod 1777 /tmp/.X11-unix \
 && useradd --create-home --uid 10001 ptb \
 && mkdir -p /data && chown ptb:ptb /data
VOLUME /data

COPY --from=build /out/ptb /usr/local/bin/ptb
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

USER ptb
WORKDIR /data

ENTRYPOINT ["/usr/bin/tini", "-s", "--", "/usr/local/bin/docker-entrypoint.sh"]
CMD ["ptb", "serve"]
