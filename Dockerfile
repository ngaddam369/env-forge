FROM golang:1.26.1-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /bin/forge-api ./cmd/forge-api

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /bin/forge-api /forge-api

ENTRYPOINT ["/forge-api"]
