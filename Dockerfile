FROM golang:1.23-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /kubevirt-online-resize-helper .

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /kubevirt-online-resize-helper /
USER 65532:65532
ENTRYPOINT ["/kubevirt-online-resize-helper"]
