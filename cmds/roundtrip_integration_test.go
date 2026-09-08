package cmds

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/olivere/elastic/v7"
)

// withImportDefaults は import のグローバル変数をテスト用に設定して戻す。
func withImportDefaults(t *testing.T) {
	t.Helper()
	orig := enableGzip
	t.Cleanup(func() { enableGzip = orig })
	enableGzip = true
}

// Test_RoundTrip は export した出力を import で書き戻せることを確認する。
//
// export のフォーマットと import のパーサは hitItem を介して対になっている。
// 片方だけを変えると、export は成功して import が黙って落とす、あるいは
// ID が変わって上書きにならないという壊れ方をする。件数と ID の一致まで
// 見ないと検出できない。
//
// sliced scroll を通した出力でも同じであることを確認する。スライスごとの
// 出力が混ざっても 1 行 1 ドキュメントの形は保たれる必要がある。
func Test_RoundTrip(t *testing.T) {
	esUrl := requireEs(t)

	tests := []struct {
		name   string
		docs   int
		slices int
	}{
		{name: "単一 scroll", docs: 250, slices: 1},
		{name: "sliced", docs: 250, slices: 3},
		// bulk のバッチ境界は 1000 件である。これを跨がないと最後の
		// flush の経路を通らない。
		{name: "bulk バッチ境界を跨ぐ", docs: 2500, slices: 3},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			withExportDefaults(t)
			withImportDefaults(t)
			Slices = tt.slices

			srcIndex := fmt.Sprintf("esdump_test_rt_src_%d_%d", tt.docs, tt.slices)
			dstIndex := fmt.Sprintf("esdump_test_rt_dst_%d_%d", tt.docs, tt.slices)
			seedIndex(t, esUrl, srcIndex, tt.docs, 3)
			// 書き戻し先は空で作る。既存の残りが混ざると件数が合わなくなる。
			seedIndex(t, esUrl, dstIndex, 0, 3)

			dump := filepath.Join(t.TempDir(), "dump.json.gz")
			if err := ExportData(dump, esUrl, srcIndex, ""); err != nil {
				t.Fatalf("ExportData がエラーを返した: %s", err)
			}

			if err := ImportData(dump, esUrl, dstIndex); err != nil {
				t.Fatalf("ImportData がエラーを返した: %s", err)
			}

			if got := countDocs(t, esUrl, dstIndex); got != int64(tt.docs) {
				t.Fatalf("import 後の件数が合わない: want %d, got %d", tt.docs, got)
			}

			// 書き戻した先を再度 export し、ID と内容が保たれていることを見る。
			// 件数だけでは ID が振り直されていても気づけない。
			redump := filepath.Join(t.TempDir(), "redump.json.gz")
			if err := ExportData(redump, esUrl, dstIndex, ""); err != nil {
				t.Fatalf("2 度目の ExportData がエラーを返した: %s", err)
			}
			got, err := readExported(t, redump, true)
			if err != nil {
				t.Fatalf("2 度目の export の読み出しに失敗した: %s", err)
			}
			if len(got) != tt.docs {
				t.Fatalf("2 度目の export の件数が合わない: want %d, got %d", tt.docs, len(got))
			}
			for i := 0; i < tt.docs; i++ {
				id := strconv.Itoa(i)
				doc, ok := got[id]
				if !ok {
					t.Errorf("ドキュメント ID %s が失われた", id)
					continue
				}
				if doc.N != i {
					t.Errorf("ID %s の n が壊れた: want %d, got %d", id, i, doc.N)
				}
			}
		})
	}
}

// Test_RoundTrip_NoGzip は --gzip=false でのラウンドトリップを確認する。
//
// export と import の gzip 分岐は独立している。片方だけ切ると読めなくなる
// ため、非 gzip でも往復できることを固定する。
func Test_RoundTrip_NoGzip(t *testing.T) {
	esUrl := requireEs(t)

	const docs = 150
	srcIndex := "esdump_test_rt_nogzip_src"
	dstIndex := "esdump_test_rt_nogzip_dst"
	seedIndex(t, esUrl, srcIndex, docs, 1)
	seedIndex(t, esUrl, dstIndex, 0, 1)

	withExportDefaults(t)
	withImportDefaults(t)
	enableGzip = false

	dump := filepath.Join(t.TempDir(), "dump.json")
	if err := ExportData(dump, esUrl, srcIndex, ""); err != nil {
		t.Fatalf("ExportData がエラーを返した: %s", err)
	}
	if err := ImportData(dump, esUrl, dstIndex); err != nil {
		t.Fatalf("ImportData がエラーを返した: %s", err)
	}
	if got := countDocs(t, esUrl, dstIndex); got != int64(docs) {
		t.Errorf("import 後の件数が合わない: want %d, got %d", docs, got)
	}
}

// Test_ExportData_FetchError は fetch が失敗したときに ExportData が
// エラーを返すことを確認する。
//
// 以前は log.Fatalln で os.Exit していた。os.Exit は defer を実行しないため
// gzip writer の Close とバッファの Flush が飛び、それまでに書いた分も
// 失われて出力が不完全な gzip になる。数時間かけた export が 1 度の
// エラーで全損する。テストプロセスも即死するため、この経路はテスト自体が
// 書けなかった。
func Test_ExportData_FetchError(t *testing.T) {
	esUrl := requireEs(t)

	withExportDefaults(t)

	// 存在しないインデックスを指定する。index_not_found_exception は
	// 空インデックスの 400 とは別物であり、飲み込まずにエラーとして返る
	// 必要がある。
	out := filepath.Join(t.TempDir(), "export.json.gz")
	err := ExportData(out, esUrl, testIndexPrefix+"no_such_index_xyz", "")
	if err == nil {
		t.Fatal("存在しないインデックスでエラーが返らなかった")
	}

	// エラーの中身まで確認する。err != nil だけでは、ファイルのオープン
	// 失敗など別の理由で通ってしまい、scroll のエラーが伝わることを
	// 確認できない。
	var esErr *elastic.Error
	if !errors.As(err, &esErr) {
		t.Fatalf("*elastic.Error が返らなかった: %#v", err)
	}
	if esErr.Status != http.StatusNotFound {
		t.Errorf("404 を期待した: got %d", esErr.Status)
	}
	if esErr.Details == nil || esErr.Details.Type != "index_not_found_exception" {
		t.Errorf("index_not_found_exception を期待した: got %#v", esErr.Details)
	}
	t.Logf("期待通りエラーが返った: %s", err)
}

// Test_ExportData_EmptyIndexSliced は一度もドキュメントが入っていない
// インデックスへの sliced scroll が 0 件で正常終了することを確認する。
//
// OpenSearch はスライスの分割に既定で _id のフィールドデータを使う。
// 空のセグメントには _id が無いため "field _id not found" の 400 になる。
// 空を読んだ結果が 0 件であることは変わらないため、エラーにはしない。
//
// 一度ドキュメントを入れて全件削除した場合はセグメントに _id が残るため
// 発生しない。発生するのは新規作成して一度も書き込んでいない場合だけで、
// 空インデックスを対象に export を走らせる運用で踏む。
func Test_ExportData_EmptyIndexSliced(t *testing.T) {
	esUrl := requireEs(t)

	for _, slices := range []int{2, 3, 8} {
		slices := slices
		t.Run(fmt.Sprintf("slices=%d", slices), func(t *testing.T) {
			withExportDefaults(t)
			Slices = slices

			indexName := fmt.Sprintf("esdump_test_empty_sliced_%d", slices)
			seedIndex(t, esUrl, indexName, 0, 3)

			out := filepath.Join(t.TempDir(), "export.json.gz")
			if err := ExportData(out, esUrl, indexName, ""); err != nil {
				t.Fatalf("空インデックスの export がエラーを返した: %s", err)
			}
			got, err := readExported(t, out, true)
			if err != nil {
				t.Fatalf("export の読み出しに失敗した: %s", err)
			}
			if len(got) != 0 {
				t.Errorf("空インデックスから %d 件が出力された", len(got))
			}
		})
	}
}
