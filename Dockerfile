FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go index.html hero.jpg ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /mealapp .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /mealapp /usr/local/bin/mealapp
COPY glovo_catalog.json ./
ENV PORT=8080
EXPOSE 8080
CMD ["sh", "-c", "exec mealapp -port ${PORT} -host 0.0.0.0"]
