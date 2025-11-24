FROM golang:1.25-alpine AS build

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -o app .

FROM alpine:3.19
WORKDIR /app
COPY --from=build /app/app .
COPY index.html .

ENV PORT=8080
EXPOSE 8080

CMD ["./app"]
