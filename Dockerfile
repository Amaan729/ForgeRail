FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/forgerail ./cmd/forgerail

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/forgerail /forgerail
EXPOSE 8080 9090
ENTRYPOINT ["/forgerail"]
