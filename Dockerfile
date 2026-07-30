FROM golang:1.22-bookworm

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -o /usr/local/bin/trait .

ENV PORT=6688
EXPOSE 6688

CMD ["sh", "-c", "mkdir -p config data public/uploads && test -f config/.env || trait init -env config/.env; exec trait serve -env config/.env"]
