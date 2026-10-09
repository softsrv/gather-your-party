FROM golang:1.27.1-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /app/server .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /app/server ./server
COPY --from=build /src/static/ ./static/
ENV LISTEN_ADDR=8080
EXPOSE 8080
ENTRYPOINT ["./server"]
