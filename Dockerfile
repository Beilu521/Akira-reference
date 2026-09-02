# 多阶段构建:编译镜像里只留最终二进制,最终镜像 ~20MB
# sqlite 用的是 modernc.org/sqlite(纯 Go),CGO_ENABLED=0 即可交叉编译静态二进制
FROM golang:1.26-alpine AS builder
WORKDIR /src
# 先拷贝 go.mod/go.sum 再下载依赖:这一层被 Docker 缓存,改代码不用重新拉依赖
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/akira ./cmd/akira

FROM alpine:3.20
COPY --from=builder /out/akira /usr/local/bin/akira
ENTRYPOINT ["akira"]
# 默认跑 demo;实际使用:docker run -v /path/logs:/logs akira watch /logs/app.log
CMD ["demo"]
