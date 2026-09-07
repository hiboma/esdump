# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 概要

esdumpはElasticsearchのデータを高速でインポート/エクスポートするためのGoで書かれたCLIツールです。nodejs版のelasticsearch-dumpより最大84倍高速で動作します。

## 開発コマンド

### ビルド
```bash
go build
```

### 特定プラットフォーム向けビルド
```bash
make darwin-amd64    # macOS 64bit版
make linux-amd64     # Linux 64bit版  
make windows-amd64   # Windows 64bit版
make all            # 主要プラットフォーム (linux-amd64, darwin-amd64, windows-amd64)
```

### リリース用ビルド
```bash
make releases       # gzip/zip圧縮されたバイナリを生成
```

### リント
```bash
make lint           # golangci-lint実行
```

### テスト実行
```bash
go test             # 全テスト実行
go test -v          # 詳細出力付きテスト実行
```

### クリーンアップ
```bash
make clean          # binディレクトリ内のバイナリを削除
```

### リリース設定の検証
```bash
make release-check     # .goreleaser.yml を検証する
make release-snapshot  # publish せずに dist/ へ成果物をビルドする
```

## リリースフロー

リリースは tagpr と GoReleaser で自動化しています。

1. master に変更をマージします
2. tagpr がバージョンを上げ CHANGELOG.md を更新するリリース PR を作成または更新します
   - PR に `minor` / `major` ラベルを付けると bump の種類を制御できます。無ラベルなら patch です
3. リリース PR をマージすると tagpr が `vX.Y.Z` タグを push し、同じワークフロー実行の中で
   GoReleaser がバイナリをビルドして GitHub Release を公開します

設定ファイル:

- `.tagpr` — tagpr の設定。`release = false` にして GitHub Release の作成は GoReleaser に任せています
- `.goreleaser.yml` — ビルド対象は darwin/linux の amd64, arm64 の4種です
  - 旧ワークフローが配布していた windows-amd64 は対象外にしました
  - アーカイブ名のバージョンには `v` が付きません。タグ `v1.2.3` に対して `esdump_1.2.3_linux_amd64.tar.gz` です
- `.github/workflows/tagpr.yml` — tagpr と GoReleaser を1つのジョブで実行します

**重要**: tagpr が `GITHUB_TOKEN` で打ったタグは push イベントを発火しません
(GitHub の無限ループ防止仕様)。そのためリリースを別ワークフローに分けず、
タグを打った同じジョブ内で `steps.tagpr.outputs.tag` を条件に GoReleaser を実行しています。

## アーキテクチャ

### プロジェクト構造
```
main.go              # エントリーポイント
main_test.go         # メインパッケージのテスト
cmds/                # コマンド実装
  ├── root.go        # cobRAルートコマンド定義
  ├── cmds.go        # export/importコマンド実装
  ├── es.go          # Elasticsearchクライアント
  └── constant.go    # バージョン情報
```

### 主要コンポーネント

**CLIフレームワーク**: github.com/spf13/cobra
- `RootCmd`: ベースコマンド（`esdump`コマンド）
- `exportCmd`: データエクスポート用サブコマンド
- `importCmd`: データインポート用サブコマンド

**Elasticsearchクライアント**: github.com/olivere/elastic/v7
- `getEsClient()`: ElasticsearchクライアントのHTTP設定（TLS無効化含む）を初期化
- `GetEsScrollService()`: エクスポート用のスクロール検索サービス
- `GetEsIndexService()`: インポート用のバルクインデックスサービス

**データ処理の特徴**:
- エクスポート時はJSONデコード/エンコードを避け、生バイトをgzipストリームに直接書き込み
- インポート時は1000件ずつバッチ処理でElasticsearchに送信
- 並行処理でフェッチとストア処理を分離（channelを使用）

### データフォーマット
```json
{"ID":"163820696","RawData":{"id":163820696,"asset":"","imageUrl":""}}
```
- `ID`: ドキュメントID
- `RawData`: 元のElasticsearchドキュメント（json.RawMessage）

### 使用方法
```bash
# エクスポート
./esdump export --index my_index -o ./my_index.json.gz

# インポート  
./esdump import --index my_index1 -i ./my_index.json.gz

# パイプライン処理
./esdump export --es http://server1:9200 -o - --index tmp_index | \
ssh server2 ./esdump import --es http://localhost:9200 --index tmp_index1 -i -
```

## 重要な実装詳細

- **高速化の要因**: JSON処理を最小限に抑え、バイト単位での直接処理を実行
- **TLS設定**: `InsecureSkipVerify: true`でTLS証明書検証を無効化
- **並行処理**: データフェッチとファイル書き込みを別goroutineで実行
- **エラーハンドリング**: バルクインポート時のエラーはログ出力のみで処理継続
