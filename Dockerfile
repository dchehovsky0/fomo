FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/fomobot ./cmd/fomobot

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends chromium ca-certificates fonts-liberation tzdata \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=build /out/fomobot /usr/local/bin/fomobot
COPY config.yaml /app/config.yaml
ENV FOMO_CHROME_PATH=/usr/bin/chromium \
    FOMO_CHROME_NO_SANDBOX=true
VOLUME ["/app/data", "/app/chrome-profile"]
EXPOSE 8080
ENTRYPOINT ["fomobot", "-config", "/app/config.yaml"]
