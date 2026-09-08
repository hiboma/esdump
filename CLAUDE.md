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
make test/short        # ユニットテストのみ。OpenSearch 不要
make test/integration  # OpenSearch を起動して全テストを実行する
make test              # 全テスト。OpenSearch に繋がらなければ統合テストはスキップ
make test/down         # コンテナを停止して削除する
```

統合テストは compose.yml の OpenSearch (127.0.0.1:19217) に接続します。
sliced scroll の全件性とページ境界は実際の scroll API を叩かないと検証
できないため、モックではなく実コンテナを使います。

- ポートは 9200 を避けています。開発機の他の Elasticsearch / OpenSearch と
  衝突させないためです
- `ESDUMP_TEST_ES` で接続先を上書きできます
- `ESDUMP_TEST_ES_REQUIRED=1` を立てると、繋がらない場合をスキップではなく
  失敗として扱います。CI で統合テストが静かにスキップされ続けるのを防ぐため、
  `.github/workflows/test.yml` はこれを立てています

#### 接続先のガード

統合テストは `seedIndex` で `DeleteIndex` を実行します。復旧不能な操作である
ため、2 段のガードを入れています。

- **接続先はローカルに限定します**。`ESDUMP_TEST_ES` が `127.0.0.1`、
  `localhost`、`::1` 以外を指す場合、テストは実行前に停止します。意図して
  リモートを対象にする場合は `ESDUMP_TEST_ALLOW_REMOTE=1` を渡します
- **インデックス名は `esdump_test_` で始めます**。`seedIndex` が強制します。
  接続先を間違えた場合でも、削除される範囲をテスト専用の名前空間に
  限定するためです

health チェックはガードになりません。到達できる場合に通すため、本番
クラスタほど確実に通過します。ホスト名で判定する必要があります。

### 手動での性能検証

統合テストの seed は数百件で、スループットの傾向を見るには足りません。
大きなインデックスは `script/seed.sh` で作ります。

```bash
make test/seed COUNT=1000000 SHARDS=5
./esdump export --es http://127.0.0.1:19217 --index esdump_bench \
  --size 1000 --slices 5 -o /tmp/bench.json.gz
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
main.go                            # エントリーポイント
main_test.go                       # ビルドとサブコマンド登録のテスト
compose.yml                        # 統合テスト用の OpenSearch
script/seed.sh                     # 手動検証用のインデックスを seed する
cmds/                              # コマンド実装
  ├── root.go                      # cobra ルートコマンド定義
  ├── cmds.go                      # export/import コマンド実装
  ├── es.go                        # Elasticsearch クライアント
  ├── constant.go                  # バージョン情報
  ├── unit_test.go                 # ES 不要のユニットテスト
  ├── integration_helper_test.go   # seed と export 読み出しのヘルパ
  ├── export_integration_test.go   # 全件性、ページ境界、MaxDocs、絞り込み
  └── roundtrip_integration_test.go # export → import → 再 export の往復
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

### export のエラー処理で守るべきこと

以下は統合テストの追加時に発見して修正した問題です。同じ形の変更を
入れると再発します。

- **`fetchSlice` で `log.Fatal` を呼ばない**

  `os.Exit` は `ExportData` の defer を実行しません。gzip writer の `Close` と
  `bufio.Writer` の `Flush` が飛ぶため、それまでに取得したデータが失われ、
  出力は途中で切れた不完全な gzip ストリームになります。数時間かけた export
  の終盤で 1 度エラーが出ただけで成果物全体が使えなくなります。
  エラーは戻り値で返し、`ExportData` が `errors.Join` で集約します。

- **書き出しエラー後も `dataChan` は読み切る**

  受信ループで `break` すると、`fetchSlice` が `dataChan` への送信で
  ブロックしたまま止まります。`wg.Wait()` が返らず `close` も行われないため
  プロセスがハングします。バッファは 300 しかないため、大きなインデックスでは
  確実にこの状態になります。エラー後は書き出しをやめてチャネルだけを
  読み進めます。

- **`exportCmd` と `importCmd` は `RunE` を使う**

  cobra の `Run` は戻り値を持ちません。`Run` に戻すとエラーが伝わらず、
  終了コードが常に 0 になります。一部のスライスが失敗して全件揃っていない
  場合に、シェルや cron、CI から成功と区別できなくなります。

- **`Flush` と `Close` のエラーは名前付き戻り値へ書き戻す**

  4MB の `bufio` バッファと gzip の内部バッファに残ったデータは、最後の
  `Flush` と `Close` で初めてファイルへ届きます。ENOSPC や `-o -` での
  パイプ切断がここで起きると、書き出し中は成功していたため戻り値が nil の
  まま出力が壊れます。gzip のフッタ (CRC32 と ISIZE) も欠けます。

- **空インデックスへの sliced scroll は 400 になる**

  一度もドキュメントが入っていないインデックスに `--slices 2` 以上を
  指定すると、OpenSearch が `field _id not found` の 400 を返します。
  スライスの分割は既定で `_id` のフィールドデータを使いますが、空の
  セグメントには `_id` が存在しないためです。0 件として正常終了させます。

  ドキュメントを入れて全件削除した後のインデックスでは発生しません
  (セグメントに `_id` が残るため)。`--MatchBody` で 0 件に絞った場合も
  発生しません。発生するのは新規作成して一度も書き込んでいない場合だけです。

  判定は `isEmptyIndexSliceErr` で `field _id not found` まで絞ります。
  `search_phase_execution_exception` だけで判定すると、マッピングの不整合や
  不正なクエリまで 0 件として飲み込み、取りこぼしに気づけなくなります。
