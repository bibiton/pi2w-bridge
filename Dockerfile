FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/pi2w-bridge .

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/pi2w-bridge /pi2w-bridge
VOLUME ["/data"]
EXPOSE 5201
ENTRYPOINT ["/pi2w-bridge"]
