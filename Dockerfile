FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/gmailbot ./cmd/gmailbot

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gmailbot /gmailbot
WORKDIR /data
ENV GMAILBOT_DB_PATH=/data/gmailbot.db
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/gmailbot"]
