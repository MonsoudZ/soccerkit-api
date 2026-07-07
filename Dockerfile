# ---- build stage ----
FROM golang:1.24 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/api ./cmd/api

# ---- runtime stage ----
FROM gcr.io/distroless/static-debian12
WORKDIR /
COPY --from=build /out/api /api
EXPOSE 3000
USER nonroot:nonroot
ENTRYPOINT ["/api"]
