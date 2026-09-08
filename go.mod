module esdump

go 1.21

// setup-go は go-version-file で go.mod を読む。toolchain がないと
// go 1.21 の記述に従って古い stdlib でビルドされ、修正済みの
// stdlib 脆弱性を含んだバイナリを配布してしまう。
// 更新は Dependabot の gomod エコシステムが追う。
toolchain go1.26.8

require (
	github.com/olivere/elastic/v7 v7.0.24
	github.com/spf13/cobra v1.1.3
)

require (
	github.com/inconshreveable/mousetrap v1.0.0 // indirect
	github.com/josharian/intern v1.0.0 // indirect
	github.com/mailru/easyjson v0.7.7 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/spf13/pflag v1.0.5 // indirect
)
