package cmds

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
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

// testIndexPrefix は統合テストが作るインデックス名の接頭辞である。
//
// seedIndex はこの接頭辞を強制する。向け先を間違えた場合でも、削除される
// 範囲をテスト専用の名前空間に閉じ込めるためである。
const testIndexPrefix = "esdump_test_"

// localTestHosts は seedIndex の削除を無条件に許すホストである。
var localTestHosts = map[string]bool{
	"127.0.0.1": true,
	"localhost": true,
	"::1":       true,
}

// assertLocalTestEndpoint は接続先がローカルであることを確認する。
//
// 統合テストは seedIndex で DeleteIndex を実行する。これは復旧不能な操作で
// あり、スナップショットがなければ元に戻せない。
//
// ESDUMP_TEST_ES に本番やステージングのエンドポイントが入ったまま
// go test を打つと、そのクラスタのインデックスが消える。go test は最も
// 気軽に、確認なしで実行されるコマンドである。
//
// health チェックはガードにならない。到達できる場合に通すため、本番
// クラスタほど確実に通過する。ホスト名で判定する必要がある。
//
// ローカル以外を意図して使う場合は ESDUMP_TEST_ALLOW_REMOTE=1 を渡す。
// 明示的な opt-in を要求し、事故と意図を区別する。
func assertLocalTestEndpoint(t *testing.T, esUrl string) {
	t.Helper()
	if err := checkLocalTestEndpoint(esUrl); err != nil {
		t.Fatalf("%s", err)
	}
}

// checkLocalTestEndpoint は接続先がローカルかどうかを判定する。
//
// assertLocalTestEndpoint から t.Fatalf する部分を分けている。ガード自体が
// 壊れると事故を防げなくなるため、判定をテストできる形にしておく。
//
// ホスト名の完全一致で判定する。文字列の部分一致にすると
// 127.0.0.1.example.com のような別のホストを通してしまう。
func checkLocalTestEndpoint(esUrl string) error {
	u, err := url.Parse(esUrl)
	if err != nil {
		return fmt.Errorf("接続先 URL を解析できない (%s): %w", esUrl, err)
	}
	if localTestHosts[u.Hostname()] {
		return nil
	}
	// 値は 1 だけを受ける。true や yes を通すと、書き間違いが意図した
	// opt-in として扱われる。
	if os.Getenv("ESDUMP_TEST_ALLOW_REMOTE") == "1" {
		return nil
	}
	return fmt.Errorf("統合テストはインデックスを削除する。ローカル以外 (%s) を対象にするには "+
		"ESDUMP_TEST_ALLOW_REMOTE=1 が必要である", esUrl)
}

// esRequired は OpenSearch に到達できない場合を失敗として扱うかを返す。
//
// 値は 1 だけを受ける。ESDUMP_TEST_ALLOW_REMOTE と揃える。空でないことを
// 条件にすると ESDUMP_TEST_ES_REQUIRED=0 が有効になり、無効化したつもりの
// 指定が有効として扱われる。
func esRequired() bool {
	return os.Getenv("ESDUMP_TEST_ES_REQUIRED") == "1"
}

// skipOrFail は OpenSearch が使えない理由を、スキップか失敗として扱う。
//
// ESDUMP_TEST_ES_REQUIRED=1 なら失敗させる。CI で統合テストが静かに
// スキップされ続け、green のまま検証が止まるのを防ぐためである。
func skipOrFail(t *testing.T, format string, args ...interface{}) {
	t.Helper()
	if esRequired() {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

// requireEs は OpenSearch に到達できなければテストをスキップする。
//
// -short でのスキップと、コンテナ未起動でのスキップを分けている。
// 前者は意図的な省略、後者は環境の不足である。CI で後者を失敗させたい
// 場合は ESDUMP_TEST_ES_REQUIRED=1 を渡す。
//
// 接続先の検証は到達確認より先に行う。ローカル以外を弾くのが目的であり、
// 到達できたかどうかとは無関係に判定しなければならない。
func requireEs(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("OpenSearch に接続するため -short ではスキップする")
	}

	url := testEsUrl()
	assertLocalTestEndpoint(t, url)

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url + "/_cluster/health")
	if err != nil {
		skipOrFail(t, "OpenSearch (%s) に接続できない (make test/up で起動する): %s", url, err)
		return ""
	}
	defer resp.Body.Close()
	// health が 200 以外の場合もスキップの判定を通す。
	//
	// ポートは繋がるがクラスタが不健全という状態がある。起動途中、OOM 後の
	// red、といった場合である。GitHub Actions の service container は
	// コンテナ起動と同時にポートを公開するため、この状態に入りやすい。
	//
	// ここを無条件のスキップにすると、CI が全テストをスキップして green を
	// 返す。ESDUMP_TEST_ES_REQUIRED はまさにこれを防ぐための指定である。
	if resp.StatusCode != http.StatusOK {
		skipOrFail(t, "OpenSearch (%s) の health が %d を返した", url, resp.StatusCode)
		return ""
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

	// 削除する前に接続先とインデックス名を検証する。DeleteIndex は復旧
	// 不能であり、呼ぶ前に弾く以外に安全策はない。
	//
	// requireEs でも接続先を検証しているが、ここでも行う。seedIndex を
	// requireEs を通さずに呼ぶテストが追加されても、削除の直前で止まる。
	assertLocalTestEndpoint(t, esUrl)
	if !strings.HasPrefix(indexName, testIndexPrefix) {
		t.Fatalf("テスト用インデックス名は %q で始める必要がある: %s", testIndexPrefix, indexName)
	}

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
	// Errors が立っていれば必ずエラーを返す。Failed() の中身を見るループ
	// だけで判定すると、Errors が真でも Failed() が空のレスポンスを成功と
	// して扱う。seed が意図通りでないまま export の件数を比べることになる。
	if res.Errors {
		if failed := res.Failed(); len(failed) > 0 {
			return fmt.Errorf("bulk item error: %v", failed[0].Error)
		}
		return fmt.Errorf("bulk レスポンスが errors=true を返したが失敗した項目が無い")
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
