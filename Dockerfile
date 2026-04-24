FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod go.sum* ./
RUN go mod download

COPY . .
RUN go build -o reachistorian ./main.go

FROM alpine:3.22

WORKDIR /app

COPY --from=builder /app/reachistorian ./reachistorian

CMD ["./reachistorian"]
