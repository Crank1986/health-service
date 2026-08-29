#образ с компилятором Go
FROM golang:1.21-alpine AS builder
WORKDIR /build
COPY main.go .
RUN go build -o server main.go

#Собираем новый образ, чистый без Go компилятора
FROM alpine:latest

##Добавляем непривелигированного пользователя
RUN addgroup -g 1001 -S appuser && adduser -u 1001 -S appuser -G appuser

WORKDIR /app
COPY --from=builder /build/server ./server

USER appuser

EXPOSE 8000
CMD [ "./server" ]