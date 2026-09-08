package cmds

import (
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
)

// Test_ExportData_AllDocs は seed した全件が export されることを確認する。
//
// sliced scroll は「全スライスの和が全件」であることが前提である。この前提が
// 崩れると取りこぼしや重複が起きるが、単一プロセスのログからは気づけない。
// 件数と内容の両方を突き合わせるのが唯一の検出手段である。
//
// スライス数はシャード数と一致する場合、それより少ない場合、多い場合を
// すべて通す。シャード数より多いスライスは OpenSearch 側で空を返す仕様で
// あり、最終ページ判定 (len(hits) < pageSize) と噛み合うかを確認する。
func Test_ExportData_AllDocs(t *testing.T) {
	esUrl := requireEs(t)

	const shards = 3
	tests := []struct {
		name     string
		docs     int
		pageSize int
		slices   int
	}{
		{name: "単一 scroll / 端数あり", docs: 250, pageSize: 100, slices: 1},
		{name: "単一 scroll / 1 ページに収まる", docs: 50, pageSize: 100, slices: 1},
		{name: "単一 scroll / 件数が size の倍数", docs: 300, pageSize: 100, slices: 1},
		{name: "単一 scroll / 件数と size が同数", docs: 100, pageSize: 100, slices: 1},
		{name: "単一 scroll / 0 件", docs: 0, pageSize: 100, slices: 1},
		{name: "sliced / スライス数 = シャード数", docs: 250, pageSize: 100, slices: 3},
		{name: "sliced / スライス数 < シャード数", docs: 250, pageSize: 100, slices: 2},
		{name: "sliced / スライス数 > シャード数", docs: 250, pageSize: 100, slices: 5},
		{name: "sliced / 件数が size の倍数", docs: 300, pageSize: 10, slices: 3},
		{name: "sliced / size が小さくページ数が多い", docs: 500, pageSize: 7, slices: 3},
		{name: "sliced / 0 件", docs: 0, pageSize: 100, slices: 3},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			withExportDefaults(t)
			PageSize = tt.pageSize
			Slices = tt.slices

			indexName := fmt.Sprintf("esdump_test_all_%d_%d_%d", tt.docs, tt.pageSize, tt.slices)
			seedIndex(t, esUrl, indexName, tt.docs, shards)

			out := filepath.Join(t.TempDir(), "export.json.gz")
			if err := ExportData(out, esUrl, indexName, ""); err != nil {
				t.Fatalf("ExportData がエラーを返した: %s", err)
			}

			got, err := readExported(t, out, true)
			if err != nil {
				t.Fatalf("export の読み出しに失敗した: %s", err)
			}
			if len(got) != tt.docs {
				t.Errorf("export された件数が合わない: want %d, got %d", tt.docs, len(got))
			}

			// 内容も突き合わせる。件数だけでは、同じ ID が重複して出力され
			// 別の ID が欠落した場合を見逃す。readExported は重複を error に
			// するため、ここでは欠落と中身の破損を見る。
			for i := 0; i < tt.docs; i++ {
				id := strconv.Itoa(i)
				doc, ok := got[id]
				if !ok {
					t.Errorf("ドキュメント ID %s が export されていない", id)
					continue
				}
				if doc.N != i {
					t.Errorf("ID %s の n が壊れている: want %d, got %d", id, i, doc.N)
				}
				if want := fmt.Sprintf("doc-%d", i); doc.Name != want {
					t.Errorf("ID %s の name が壊れている: want %s, got %s", id, want, doc.Name)
				}
			}
		})
	}
}

// Test_ExportData_MaxDocs は -c での打ち切りを確認する。
//
// MaxDocs は全スライス合計で判定する必要がある。スライスごとに数えると
// slices 倍の件数が出力される。sliced scroll では複数の goroutine が同じ
// カウンタを atomic で更新するため、上限をわずかに超えることは避けられない。
// ここでは「指定件数以上を出力しない」ではなく「全件までは出力しない」を
// 確認する。
func Test_ExportData_MaxDocs(t *testing.T) {
	esUrl := requireEs(t)

	const (
		shards = 3
		docs   = 500
	)
	indexName := "esdump_test_maxdocs"
	seedIndex(t, esUrl, indexName, docs, shards)

	tests := []struct {
		name     string
		maxDocs  int
		pageSize int
		slices   int
		// exact は出力件数が maxDocs と厳密に一致することを要求するかである。
		// 単一 scroll では 1 件ごとに判定して即 return するため一致する。
		exact bool
	}{
		{name: "単一 scroll", maxDocs: 120, pageSize: 100, slices: 1, exact: true},
		{name: "単一 scroll / ページ境界", maxDocs: 100, pageSize: 100, slices: 1, exact: true},
		// pageSize は小さく取る。許容上限は maxDocs + slices*pageSize であり、
		// pageSize が大きいと上限が緩くなって per-slice で数えるバグを
		// 通してしまう。pageSize 100 では上限が 420 になり、全件 500 を
		// 出力しない限り検出できない。
		{name: "sliced", maxDocs: 120, pageSize: 10, slices: 3, exact: false},
		{name: "sliced / size が小さい", maxDocs: 50, pageSize: 7, slices: 3, exact: false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			withExportDefaults(t)
			MaxDocs = tt.maxDocs
			PageSize = tt.pageSize
			Slices = tt.slices

			out := filepath.Join(t.TempDir(), "export.json.gz")
			if err := ExportData(out, esUrl, indexName, ""); err != nil {
				t.Fatalf("ExportData がエラーを返した: %s", err)
			}

			got, err := readExported(t, out, true)
			if err != nil {
				t.Fatalf("export の読み出しに失敗した: %s", err)
			}

			if len(got) < tt.maxDocs {
				t.Errorf("MaxDocs %d に達する前に打ち切られた: got %d", tt.maxDocs, len(got))
			}
			if tt.exact {
				if len(got) != tt.maxDocs {
					t.Errorf("MaxDocs %d を超えて出力された: got %d", tt.maxDocs, len(got))
				}
				return
			}
			// sliced では上限をまたぐ分の超過を許すが、全件になってはいけない。
			// 全件になるのは MaxDocs の判定が効いていないことを意味する。
			if len(got) >= docs {
				t.Errorf("MaxDocs %d が効かず全件 (%d) が出力された: got %d", tt.maxDocs, docs, len(got))
			}
			// スライスごとに数えていると slices 倍まで出る。上限の目安として
			// maxDocs + slices*pageSize を超えたら判定が共有されていないと見る。
			if limit := tt.maxDocs + tt.slices*tt.pageSize; len(got) > limit {
				t.Errorf("MaxDocs %d に対して超過が大きすぎる (limit %d): got %d", tt.maxDocs, limit, len(got))
			}
		})
	}
}

// Test_ExportData_MatchBody は -m での絞り込みを確認する。
//
// matchBody は RawStringQuery としてそのまま渡される。sliced scroll と
// 併用したときにクエリが全スライスへ適用されるかを確認する。片方だけに
// 適用されると、絞り込んだつもりが全件になる。
func Test_ExportData_MatchBody(t *testing.T) {
	esUrl := requireEs(t)

	const (
		shards = 3
		docs   = 300
	)
	indexName := "esdump_test_matchbody"
	seedIndex(t, esUrl, indexName, docs, shards)

	// n >= 200 の 100 件だけを対象にする。
	const matchBody = `{"range":{"n":{"gte":200}}}`

	for _, slices := range []int{1, 3} {
		slices := slices
		t.Run(fmt.Sprintf("slices=%d", slices), func(t *testing.T) {
			withExportDefaults(t)
			Slices = slices
			PageSize = 30

			out := filepath.Join(t.TempDir(), "export.json.gz")
			if err := ExportData(out, esUrl, indexName, matchBody); err != nil {
				t.Fatalf("ExportData がエラーを返した: %s", err)
			}

			got, err := readExported(t, out, true)
			if err != nil {
				t.Fatalf("export の読み出しに失敗した: %s", err)
			}
			if len(got) != 100 {
				t.Errorf("絞り込みの件数が合わない: want 100, got %d", len(got))
			}
			for id, doc := range got {
				if doc.N < 200 {
					t.Errorf("絞り込みの対象外が export された: ID %s, n %d", id, doc.N)
				}
			}
		})
	}
}

// Test_ExportData_NoGzip は --gzip=false での出力を確認する。
//
// gzip を切ると拡張子の .gz 付与も行われない。この分岐は export と import で
// 対称になっている必要がある。
func Test_ExportData_NoGzip(t *testing.T) {
	esUrl := requireEs(t)

	const docs = 150
	indexName := "esdump_test_nogzip"
	seedIndex(t, esUrl, indexName, docs, 1)

	withExportDefaults(t)
	enableGzip = false

	out := filepath.Join(t.TempDir(), "export.json")
	if err := ExportData(out, esUrl, indexName, ""); err != nil {
		t.Fatalf("ExportData がエラーを返した: %s", err)
	}

	got, err := readExported(t, out, false)
	if err != nil {
		t.Fatalf("export の読み出しに失敗した: %s", err)
	}
	if len(got) != docs {
		t.Errorf("export された件数が合わない: want %d, got %d", docs, len(got))
	}
}

// Test_ExportData_GzipSuffix は .gz が付いていない出力先に .gz が付与される
// ことを確認する。
//
// gzip を有効にしたまま .gz なしのパスを渡すと、実装は拡張子を足す。
// 呼び出し側が渡したパスと実際の出力先が違うため、テストで固定する。
func Test_ExportData_GzipSuffix(t *testing.T) {
	esUrl := requireEs(t)

	const docs = 20
	indexName := "esdump_test_gzipsuffix"
	seedIndex(t, esUrl, indexName, docs, 1)

	withExportDefaults(t)

	dir := t.TempDir()
	out := filepath.Join(dir, "export.json")
	if err := ExportData(out, esUrl, indexName, ""); err != nil {
		t.Fatalf("ExportData がエラーを返した: %s", err)
	}

	got, err := readExported(t, out+".gz", true)
	if err != nil {
		t.Fatalf("%s.gz の読み出しに失敗した: %s", out, err)
	}
	if len(got) != docs {
		t.Errorf("export された件数が合わない: want %d, got %d", docs, len(got))
	}
}
