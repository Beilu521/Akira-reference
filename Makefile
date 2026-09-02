APP      := akira
VERSION  ?= dev
LDFLAGS  := -ldflags "-X main.version=$(VERSION)"

.PHONY: build build-windows build-linux build-darwin release test race cover vet fmt fuzz run-demo clean

build: ## 编译当前平台
	go build $(LDFLAGS) -o bin/$(APP) ./cmd/akira

# ===== 跨平台打包:make release → dist/ 下三个平台的单文件二进制 =====
# 这就是 Go 的"npm 包"——一个可执行文件扔到机器上就能跑,不需要运行时/依赖管理器
build-windows:
	GOOS=windows GOARCH=amd64 go build $(LDFLAGS) -o dist/$(APP)-windows-amd64.exe ./cmd/akira

build-linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build $(LDFLAGS) -o dist/$(APP)-linux-amd64 ./cmd/akira

build-darwin:
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build $(LDFLAGS) -o dist/$(APP)-darwin-arm64 ./cmd/akira

release: clean build-windows build-linux build-darwin ## 一键打包全平台
	@echo "产物已生成在 dist/ 目录"

test:
	go test ./...

# 竞态检测:并发代码(watch 的 worker/channel)必须过这一关
race:
	go test -race ./...

# 覆盖率:输出每个包/函数的覆盖率,尾部一行是总覆盖率
cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

# 模糊测试:随机"模式×日志行"轰炸匹配器,验证分类优化语义等价
fuzz:
	go test -fuzz=FuzzMatcherEquivalence -fuzztime=60s ./internal/detector/

vet:
	go vet ./...

fmt:
	gofmt -l -w .

run-demo:
	go run ./cmd/akira demo

clean:
	rm -rf bin dist coverage.out
