package cmds

import (
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bulk のレスポンスを差し替えられる偽の ES を立てて、実クラスタでは
// 起こしにくい応答を検証する。OpenSearch を起動せずに走るため -short でも
// 実行される。
func fakeEs(t *testing.T, bulkStatus int, bulkBody string) string {
	t.Helper()
	mux := http.NewServeMux()
	// olivere/elastic は起動時にバージョンを確認する。
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/_bulk") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(bulkStatus)
			_, _ = w.Write([]byte(bulkBody))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":{"number":"7.10.2"}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func writeDump(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dump.json.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("テスト用ファイルの作成に失敗した: %s", err)
	}
	defer f.Close()
	zw := gzip.NewWriter(f)
	for _, line := range lines {
		if _, err := zw.Write([]byte(line + "\n")); err != nil {
			t.Fatalf("書き込みに失敗した: %s", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip の Close に失敗した: %s", err)
	}
	return path
}

// Test_ImportData_BulkRequestFails は bulk リクエスト自体が失敗したときに
// 中断することを確認する。
//
// 接続断や HTTP エラーは以降も失敗し続ける。個々のドキュメントの拒否とは
// 違い、続行しても意味がない。
func Test_ImportData_BulkRequestFails(t *testing.T) {
	esUrl := fakeEs(t, http.StatusInternalServerError, `{"error":"boom"}`)
	withImportDefaults(t)

	dump := writeDump(t, `{"_id":"a","_source":{"n":1}}`)

	err := ImportData(dump, esUrl, "esdump_test_bulkfail")
	if err == nil {
		t.Fatal("bulk リクエストの失敗でエラーが返らなかった")
	}
	if !strings.Contains(err.Error(), "bulk request に失敗した") {
		t.Errorf("リクエスト失敗と分かるエラーでない: %v", err)
	}
}

// Test_ImportData_ErrorsWithoutItems は errors=true で items が空の応答を
// 成功として扱わないことを確認する。
//
// Failed() は Items が nil なら nil を返す。ループの有無だけで判定すると、
// 全件が拒否されていても 0 件の失敗として成功になる。
func Test_ImportData_ErrorsWithoutItems(t *testing.T) {
	esUrl := fakeEs(t, http.StatusOK, `{"took":1,"errors":true,"items":[]}`)
	withImportDefaults(t)

	dump := writeDump(t, `{"_id":"a","_source":{"n":1}}`)

	err := ImportData(dump, esUrl, "esdump_test_erroritems")
	if err == nil {
		t.Fatal("errors=true の応答でエラーが返らなかった")
	}
	if !strings.Contains(err.Error(), "errors=true") {
		t.Errorf("errors=true と分かるエラーでない: %v", err)
	}
}

// Test_ImportData_EmptyDump は 0 件のダンプが成功することを確認する。
//
// 空の bulk を送ると olivere/elastic は "No bulk actions to commit" を
// 返す。送らずに終える必要がある。
func Test_ImportData_EmptyDump(t *testing.T) {
	esUrl := fakeEs(t, http.StatusOK, `{"took":1,"errors":false,"items":[]}`)
	withImportDefaults(t)

	dump := writeDump(t)

	if err := ImportData(dump, esUrl, "esdump_test_empty"); err != nil {
		t.Fatalf("空のダンプでエラーが返った: %s", err)
	}
}

// Test_ImportData_CountsAcrossBatches はバッチ境界をまたいでも拒否件数を
// 正しく数えることを確認する。
//
// bulk は 1000 件ごとに送る。最後の端数のバッチで数え漏らすと、報告する
// 件数が実際とずれる。
func Test_ImportData_CountsAcrossBatches(t *testing.T) {
	// 全件を拒否する応答を返す。件数は問わず、items の数だけ失敗を数える。
	var items []string
	for i := 0; i < 1000; i++ {
		items = append(items, `{"index":{"_id":"x","status":400,"error":{"type":"mapper_parsing_exception","reason":"bad"}}}`)
	}
	body := `{"took":1,"errors":true,"items":[` + strings.Join(items, ",") + `]}`
	esUrl := fakeEs(t, http.StatusOK, body)
	withImportDefaults(t)

	// 1001 行 = 1000 件のバッチと 1 件のバッチに分かれる。
	lines := make([]string, 0, 1001)
	for i := 0; i < 1001; i++ {
		lines = append(lines, `{"_id":"a","_source":{"n":1}}`)
	}
	dump := writeDump(t, lines...)

	err := ImportData(dump, esUrl, "esdump_test_batches")
	if err == nil {
		t.Fatal("拒否されたのにエラーが返らなかった")
	}
	// 偽の ES は毎回 1000 件分の失敗を返すため、2 バッチで 2000 件になる。
	// 端数のバッチも数えていることが分かればよい。
	if !strings.Contains(err.Error(), "/ 1001 件") {
		t.Errorf("読み込んだ行数が合わない: %v", err)
	}
	if !strings.Contains(err.Error(), "2000 /") {
		t.Errorf("2 バッチ分を数えていない: %v", err)
	}
}
