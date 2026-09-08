NAME=esdump
BINDIR=bin
VERSION=$(shell git describe --tags || echo "unknown version")
BUILDTIME=$(shell date -u)
GOBUILD=CGO_ENABLED=0 go build -trimpath -ldflags '-X "esdump/cmds.Version=$(VERSION)" \
		-X "esdump/cmds.BuildTime=$(BUILDTIME)" \
		-w -s -buildid='

PLATFORM_LIST = \
	darwin-amd64 \
	darwin-arm64 \
	linux-386 \
	linux-amd64 \
	linux-armv5 \
	linux-armv6 \
	linux-armv7 \
	linux-armv8 \
	linux-mips-softfloat \
	linux-mips-hardfloat \
	linux-mipsle-softfloat \
	linux-mipsle-hardfloat \
	linux-mips64 \
	linux-mips64le \
	freebsd-386 \
	freebsd-amd64 \
	freebsd-arm64

WINDOWS_ARCH_LIST = \
	windows-386 \
	windows-amd64 \
	windows-arm64 \
	windows-arm32v7

LINUX_LIST = \
	linux-amd64 \
	darwin-amd64


all: linux-amd64 darwin-amd64 windows-amd64 # Most used

docker:
	$(GOBUILD) -o $(BINDIR)/$(NAME)-$@

darwin-amd64:
	GOARCH=amd64 GOOS=darwin $(GOBUILD) -o $(BINDIR)/$(NAME)-$@

darwin-arm64:
	GOARCH=arm64 GOOS=darwin $(GOBUILD) -o $(BINDIR)/$(NAME)-$@

linux-386:
	GOARCH=386 GOOS=linux $(GOBUILD) -o $(BINDIR)/$(NAME)-$@

linux-amd64:
	GOARCH=amd64 GOOS=linux $(GOBUILD) -o $(BINDIR)/$(NAME)-$@

linux-armv5:
	GOARCH=arm GOOS=linux GOARM=5 $(GOBUILD) -o $(BINDIR)/$(NAME)-$@

linux-armv6:
	GOARCH=arm GOOS=linux GOARM=6 $(GOBUILD) -o $(BINDIR)/$(NAME)-$@

linux-armv7:
	GOARCH=arm GOOS=linux GOARM=7 $(GOBUILD) -o $(BINDIR)/$(NAME)-$@

linux-armv8:
	GOARCH=arm64 GOOS=linux $(GOBUILD) -o $(BINDIR)/$(NAME)-$@

linux-mips-softfloat:
	GOARCH=mips GOMIPS=softfloat GOOS=linux $(GOBUILD) -o $(BINDIR)/$(NAME)-$@

linux-mips-hardfloat:
	GOARCH=mips GOMIPS=hardfloat GOOS=linux $(GOBUILD) -o $(BINDIR)/$(NAME)-$@

linux-mipsle-softfloat:
	GOARCH=mipsle GOMIPS=softfloat GOOS=linux $(GOBUILD) -o $(BINDIR)/$(NAME)-$@

linux-mipsle-hardfloat:
	GOARCH=mipsle GOMIPS=hardfloat GOOS=linux $(GOBUILD) -o $(BINDIR)/$(NAME)-$@

linux-mips64:
	GOARCH=mips64 GOOS=linux $(GOBUILD) -o $(BINDIR)/$(NAME)-$@

linux-mips64le:
	GOARCH=mips64le GOOS=linux $(GOBUILD) -o $(BINDIR)/$(NAME)-$@

freebsd-386:
	GOARCH=386 GOOS=freebsd $(GOBUILD) -o $(BINDIR)/$(NAME)-$@

freebsd-amd64:
	GOARCH=amd64 GOOS=freebsd $(GOBUILD) -o $(BINDIR)/$(NAME)-$@

freebsd-arm64:
	GOARCH=arm64 GOOS=freebsd $(GOBUILD) -o $(BINDIR)/$(NAME)-$@

windows-386:
	GOARCH=386 GOOS=windows $(GOBUILD) -o $(BINDIR)/$(NAME)-$@.exe

windows-amd64:
	GOARCH=amd64 GOOS=windows $(GOBUILD) -o $(BINDIR)/$(NAME)-$@.exe

windows-arm64:
	GOARCH=arm64 GOOS=windows $(GOBUILD) -o $(BINDIR)/$(NAME)-$@.exe

windows-arm32v7:
	GOARCH=arm GOOS=windows GOARM=7 $(GOBUILD) -o $(BINDIR)/$(NAME)-$@.exe

#gz_releases=$(addsuffix .gz, $(PLATFORM_LIST))
#zip_releases=$(addsuffix .zip, $(WINDOWS_ARCH_LIST))
#
#$(gz_releases): %.gz : %
#	chmod +x $(BINDIR)/$(NAME)-$(basename $@)
#	gzip -f -S -$(VERSION).gz $(BINDIR)/$(NAME)-$(basename $@)
#
#$(zip_releases): %.zip : %
#	zip -m -j $(BINDIR)/$(NAME)-$(basename $@)-$(VERSION).zip $(BINDIR)/$(NAME)-$(basename $@).exe
#
#all-arch: $(PLATFORM_LIST) $(WINDOWS_ARCH_LIST)
#releases: $(gz_releases) $(zip_releases)

linux_releases=$(addsuffix .gz, $(LINUX_LIST))
windows_releases=$(addsuffix .zip, windows-amd64)
$(linux_releases): %.gz : %
	chmod +x $(BINDIR)/$(NAME)-$(basename $@)
	gzip -f -S -$(VERSION).gz $(BINDIR)/$(NAME)-$(basename $@)
$(windows_releases): %.zip : %
	zip -m -j $(BINDIR)/$(NAME)-$(basename $@)-$(VERSION).zip $(BINDIR)/$(NAME)-$(basename $@).exe
#only window mac  linux amd64
releases: $(linux_releases) $(windows_releases)


lint:
	golangci-lint run --disable-all -E govet -E gofumpt -E megacheck ./...

# サプライチェーン対策の検証をローカルで回す。CI の security.yml と同じ内容である。
# 手元で先に気づけるようにするためで、CI 側のゲーティングは省略しない。
.PHONY: lint/security
lint/security:
	actionlint
	zizmor .github/workflows/
	pinact run --check
	govulncheck ./...

clean:
	rm $(BINDIR)/*
# --- release (goreleaser) ---
# 実際のリリースは tag を打つと GitHub Actions が実行する。
# 以下は手元で設定と成果物を検証するためのターゲット。
.PHONY: release-check release-snapshot

release-check:
	goreleaser check

release-snapshot:
	goreleaser release --snapshot --clean --skip=publish

# --- 統合テスト (OpenSearch コンテナ) ---
#
# sliced scroll の全件性やページ境界は、実際の scroll API を叩かないと
# 検証できない。compose.yml の OpenSearch を起動してテストを走らせる。
#
# ホスト側のポートは 19217 である。9200 を避けているのは、開発機で動く
# 他の Elasticsearch / OpenSearch と衝突させないためである。
.PHONY: test test/short test/up test/down test/integration test/logs test/seed

# test は統合テストを含む全テストである。コンテナが起動していなければ
# 統合テストはスキップされる。
test:
	go test ./... -timeout 600s

# test/short は OpenSearch を必要としないテストだけを走らせる。
# 手元で素早く回すためのターゲットである。CI は test/integration と同じ
# 条件で全テストを実行する。
test/short:
	go test ./... -short -timeout 120s

test/up:
	docker compose up -d --wait

test/down:
	docker compose down -v

# test/integration は起動から実行、停止までを通す。
# ESDUMP_TEST_ES_REQUIRED=1 を付けることで、コンテナに繋がらない場合を
# スキップではなく失敗として扱う。CI で「静かにスキップされていた」を
# 防ぐためである。
test/integration: test/up
	ESDUMP_TEST_ES_REQUIRED=1 go test ./... -v -timeout 600s

test/logs:
	docker compose logs -f opensearch

# test/seed は手動検証用のインデックスを作る。件数とシャード数を渡せる。
#   make test/seed COUNT=1000000 SHARDS=5
COUNT ?= 100000
SHARDS ?= 3
test/seed: test/up
	./script/seed.sh $(COUNT) $(SHARDS)
