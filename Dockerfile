FROM golang:1.22-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/trait .

FROM debian:bookworm-slim
RUN apt-get update \
	&& apt-get install -y --no-install-recommends ca-certificates git \
	&& rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=build /out/trait /usr/local/bin/trait
COPY --from=build /src/logo.png /src/logo-nav.png ./
COPY --from=build /src/traits ./seed-traits
EXPOSE 6688
CMD ["trait", "serve", "-env", "/app/config/.env"]
