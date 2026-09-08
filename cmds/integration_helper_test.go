package cmds

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/olivere/elastic/v7"
)

// 統合テストは compose.yml の OpenSearch に接続する。
//
// URL は ESDUMP_TEST_ES で上書きできる。既定のポート 19217 は compose.yml が
// ホスト側に公開しているポートである。9200 を避けているのは、開発機で動く
// 他の Elasticsearch / OpenSearch と衝突させないためである。
const defaultTestEsUrl = "http://127.0.0.1:19217"

func testEsUrl() string {
	if v := os.Getenv("ESDUMP_TEST_ES"); v != "" {
		return v
	}
	return defaultTestEsUrl
}

// requireEs は OpenSearch に到達できなければテストをスキップする。
//
// -short でのスキップと、コンテナ未起動でのスキップを分けている。
// 前者は意図的な省略、後者は環境の不足である。CI で後者を失敗させたい
// 場合は ESDUMP_TEST_ES_REQUIRED=1 を渡す。
func requireEs(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("OpenSearch に接続するため -short ではスキップする")
	}

	url := testEsUrl()
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url + "/_cluster/health")
	if err != nil {
		if os.Getenv("ESDUMP_TEST_ES_REQUIRED") != "" {
			t.Fatalf("OpenSearch (%s) に接続できない: %s", url, err)
		}
		t.Skipf("OpenSearch (%s) に接続できないためスキップする (make test/up で起動する): %s", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Skipf("OpenSearch (%s) の health が %d を返した", url, resp.StatusCode)
	}
	return url
}

// seedDoc は seed するドキュメントの形である。
//
// n は 0 起点の連番で、export された件数と内容の突き合わせに使う。
// 欠落や重複を検出するには、ドキュメントを一意に識別できる必要がある。
type seedDoc struct {
	N    int    `json:"n"`
	Name string `json:"name"`
	Pad  string `json:"pad"`
}

// seedIndex は count 件のドキュメントを持つインデックスを作る。
//
// 既存の同名インデックスは消してから作る。テストを繰り返し実行したときに
// 前回の残りが件数に混ざるのを避けるためである。
//
// shards は sliced scroll の検証に効く。sliced scroll のスライス数を
// シャード数より多く指定したときの挙動を確認するため、呼び出し側が
// シャード数を決められるようにしている。
func seedIndex(t *testing.T, esUrl, indexName string, count, shards int) {
	t.Helper()
	ctx := context.Background()
	client := getEsClient(esUrl)

	if _, err := client.DeleteIndex(indexName).Do(ctx); err != nil {
		// 存在しないインデックスの削除は 404 になる。初回実行では正常である。
		if !elastic.IsNotFound(err) {
			t.Fatalf("インデックス %s の削除に失敗した: %s", indexName, err)
		}
	}

	body := map[string]interface{}{
		"settings": map[string]interface{}{
			"number_of_shards":   shards,
			"number_of_replicas": 0,
		},
	}
	if _, err := client.CreateIndex(indexName).BodyJson(body).Do(ctx); err != nil {
		t.Fatalf("インデックス %s の作成に失敗した: %s", indexName, err)
	}

	if count > 0 {
		bulk := client.Bulk().Index(indexName)
		for i := 0; i < count; i++ {
			doc := seedDoc{N: i, Name: fmt.Sprintf("doc-%d", i), Pad: "xxxxxxxxxxxxxxxx"}
			req := elastic.NewBulkIndexRequest().Id(strconv.Itoa(i)).Doc(doc)
			bulk = bulk.Add(req)
			// bulk は 1000 件ずつ流す。1 リクエストが大きくなりすぎると
			// OpenSearch 側の http.max_content_length に当たる。
			if bulk.NumberOfActions() >= 1000 {
				if err := flushBulk(ctx, t, bulk); err != nil {
					t.Fatalf("bulk index に失敗した: %s", err)
				}
			}
		}
		if bulk.NumberOfActions() > 0 {
			if err := flushBulk(ctx, t, bulk); err != nil {
				t.Fatalf("bulk index に失敗した: %s", err)
			}
		}
	}

	// refresh しないと直後の検索でドキュメントが見えない。OpenSearch は
	// 既定で 1 秒間隔の refresh であり、待たずに検索すると 0 件になる。
	if _, err := client.Refresh(indexName).Do(ctx); err != nil {
		t.Fatalf("refresh に失敗した: %s", err)
	}

	// 件数を確認する。seed が意図通りでないまま export の件数を比べると、
	// export 側のバグと seed 側のバグを区別できない。
	got, err := client.Count(indexName).Do(ctx)
	if err != nil {
		t.Fatalf("count に失敗した: %s", err)
	}
	if got != int64(count) {
		t.Fatalf("seed した件数が合わない: want %d, got %d", count, got)
	}
}

func flushBulk(ctx context.Context, t *testing.T, bulk *elastic.BulkService) error {
	t.Helper()
	res, err := bulk.Do(ctx)
	if err != nil {
		return err
	}
	if res.Errors {
		for _, item := range res.Failed() {
			return fmt.Errorf("bulk item error: %v", item.Error)
		}
	}
	return nil
}

// countDocs はインデックスの現在の件数を返す。
func countDocs(t *testing.T, esUrl, indexName string) int64 {
	t.Helper()
	ctx := context.Background()
	client := getEsClient(esUrl)
	if _, err := client.Refresh(indexName).Do(ctx); err != nil {
		t.Fatalf("refresh に失敗した: %s", err)
	}
	n, err := client.Count(indexName).Do(ctx)
	if err != nil {
		t.Fatalf("count に失敗した: %s", err)
	}
	return n
}

// readExported は export した gzip ファイルを読み、1 行 1 件としてパースする。
//
// 返す map のキーはドキュメント ID である。重複した ID があれば error を
// 返す。sliced scroll でスライスの範囲が重なっていると同じドキュメントが
// 2 度出力されるため、これを検出できるようにしている。
func readExported(t *testing.T, path string, gzipped bool) (map[string]seedDoc, error) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var r io.Reader = f
	if gzipped {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("gzip reader: %w", err)
		}
		defer zr.Close()
		r = zr
	}

	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}

	out := make(map[string]seedDoc)
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var item hitItem
		if err := json.Unmarshal(line, &item); err != nil {
			return nil, fmt.Errorf("行のパースに失敗した (%q): %w", string(line), err)
		}
		var doc seedDoc
		if err := json.Unmarshal(item.Source, &doc); err != nil {
			return nil, fmt.Errorf("_source のパースに失敗した (%q): %w", string(item.Source), err)
		}
		if _, dup := out[item.ID]; dup {
			return nil, fmt.Errorf("ドキュメント ID %s が重複して出力された", item.ID)
		}
		out[item.ID] = doc
	}
	return out, nil
}

// withExportDefaults は export のグローバル変数をテスト用の既定値に設定し、
// テスト終了時に元へ戻す。
//
// ExportData はフラグをパッケージ変数から読む。テストごとに設定すると
// 前のテストの値が残り、失敗の原因が追いにくくなるため t.Cleanup で戻す。
func withExportDefaults(t *testing.T) {
	t.Helper()
	orig := struct {
		maxDocs       int
		progressEvery int
		pagesEvery    int
		pageSize      int
		slices        int
		gzip          bool
	}{MaxDocs, ProgressEvery, PagesEvery, PageSize, Slices, enableGzip}

	t.Cleanup(func() {
		MaxDocs = orig.maxDocs
		ProgressEvery = orig.progressEvery
		PagesEvery = orig.pagesEvery
		PageSize = orig.pageSize
		Slices = orig.slices
		enableGzip = orig.gzip
	})

	MaxDocs = 0
	// 進捗ログはテスト出力を埋めるため既定で切る。
	ProgressEvery = 0
	PagesEvery = 0
	PageSize = 100
	Slices = 1
	enableGzip = true
}
