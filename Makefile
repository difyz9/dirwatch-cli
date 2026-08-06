# dmon Makefile
#
# 常用命令：
#   make                等价于 make build
#   make build          构建本地二进制（默认输出到项目根目录 dmon）
#   make test           运行全部测试
#   make vet / lint     go vet 静态检查
#   make fmt            格式化全部 Go 源码
#   make release        按 GitHub Actions 相同矩阵本地交叉编译到 dist/
#   make install        安装到 GOBIN（无需 sudo）
#   make uninstall      从 GOBIN 移除
#   make clean          清理构建产物
#
# 版本号：默认从 git 自动获取（最近 tag、提交距离、dirty 标记，见 VERSION 定义）；
#        无法获取时回退 dev，也可手动指定，例如 make build VERSION=v1.0.0

BINARY  := dmon
VERSION ?= $(shell git describe --tags --dirty --always 2>/dev/null || echo dev)
PKG     := github.com/difyz9/dmon-cli/cmd
LDFLAGS := -s -w -X $(PKG).version=$(VERSION)

.PHONY: build test vet fmt lint release clean install uninstall

build:
	go build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY) .

test:
	go test ./...

vet:
	go vet ./...

fmt:
	go fmt ./...

lint: vet

# 按 CI 的 release.yml 相同矩阵本地交叉编译，用于发布前自检
release:
	mkdir -p dist
	for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do \
		os="$${target%/*}"; arch="$${target#*/}"; \
		binary="$(BINARY)"; \
		[ "$$os" = "windows" ] && binary="$(BINARY).exe"; \
		dir="dist/$(BINARY)_$${os}_$${arch}"; \
		echo "build $$os/$$arch -> $$dir/$$binary"; \
		GOOS="$$os" GOARCH="$$arch" CGO_ENABLED=0 \
			go build -trimpath -ldflags="$(LDFLAGS)" -o "$$dir/$$binary" . || exit 1; \
	done

clean:
	rm -f $(BINARY)
	rm -rf dist

# 安装到当前用户的 GOBIN（或 GOPATH/bin），无需 sudo
install:
	@dir="$$(go env GOBIN)"; \
	test -n "$$dir" || dir="$$(go env GOPATH)/bin"; \
	mkdir -p "$$dir"; \
	go build -trimpath -ldflags="$(LDFLAGS)" -o "$$dir/$(BINARY)" . && \
	echo "$(BINARY) 已安装到 $$dir/$(BINARY)"

uninstall:
	@dir="$$(go env GOBIN)"; \
	test -n "$$dir" || dir="$$(go env GOPATH)/bin"; \
	rm -f "$$dir/$(BINARY)" && \
	echo "已移除 $$dir/$(BINARY)"
