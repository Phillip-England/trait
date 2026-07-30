FROM golang:1.26
WORKDIR /app
COPY . .
RUN go build -o /usr/local/bin/trait .
EXPOSE 6688
CMD ["trait"]
