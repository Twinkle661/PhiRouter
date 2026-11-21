FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w" -o /router ./cmd/router

FROM node:22-bookworm-slim
ARG PI_VERSION=1.6.0
RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && npm install -g --ignore-scripts @earendil-works/pi-coding-agent@${PI_VERSION} \
    && npm cache clean --force \
    && mkdir -p /home/node/.pi/agent /data/media/in /data/media/out /app/router \
    && chown -R node:node /home/node/.pi /data/media /app
COPY --from=build /router /usr/local/bin/router
COPY --chown=node:node config.example.yaml /app/router/config.yaml
ENV HOME=/home/node
USER node
WORKDIR /app/router
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD ["router", "-healthcheck", "http://127.0.0.1:8080/healthz"]
ENTRYPOINT ["router"]
CMD ["-config", "/app/router/config.yaml"]
