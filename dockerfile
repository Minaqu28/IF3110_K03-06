FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /server ./internal
RUN CGO_ENABLED=0 go build -trimpath -o /seed ./seed

FROM alpine:3.22
RUN addgroup -S app && adduser -S -G app app
USER app
COPY --from=build /server /server
COPY --from=build /seed /seed
EXPOSE 8080
ENTRYPOINT ["/server"]
