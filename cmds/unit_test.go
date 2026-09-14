package cmds

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/olivere/elastic/v7"
)

// Test_applyLogTimezone はタイムゾーン指定の受理と拒否を確認する。
//
// time.Local を書き換えるため、テスト終了時に元へ戻す。戻さないと後続の
// テストのログのタイムスタンプまで変わる。
func Test_applyLogTimezone(t *testing.T) {
	orig := time.Local
	t.Cleanup(func() { time.Local = orig })

	tests := []struct {
		name    string
		tz      string
		wantErr bool
		// wantLoc は設定後に time.Local が持つべき名前である。空なら
		// 変更されないことを期待する。
		wantLoc string
	}{
		{name: "空文字は変更しない", tz: "", wantErr: false, wantLoc: ""},
		{name: "Asia/Tokyo", tz: "Asia/Tokyo", wantErr: false, wantLoc: "Asia/Tokyo"},
		{name: "UTC", tz: "UTC", wantErr: false, wantLoc: "UTC"},
		{name: "America/New_York", tz: "America/New_York", wantErr: false, wantLoc: "America/New_York"},
		// 不正な名前は必ず弾く。黙って UTC などに落とすと、長時間動く
		// export のログが意図と違うタイムゾーンで残り、後から読めなくなる。
		{name: "存在しない名前", tz: "Asia/NoSuchCity", wantErr: true},
		{name: "JST は IANA 名ではない", tz: "JST", wantErr: true},
		// 相対パスによる読み出しを許すと、意図しないファイルを
		// タイムゾーン定義として読ませられる。
		{name: "パス区切りを含む不正な名前", tz: "../../etc/passwd", wantErr: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			time.Local = orig
			err := applyLogTimezone(tt.tz)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("エラーを期待したが nil が返った (tz=%q)", tt.tz)
				}
				// 失敗時に time.Local を書き換えていないことを確認する。
				// 中途半端に変えると、Fatal を握り潰した呼び出し側で
				// 不整合が残る。
				if time.Local != orig {
					t.Errorf("エラー時に time.Local が変更された: %v", time.Local)
				}
				return
			}
			if err != nil {
				t.Fatalf("エラーが返った (tz=%q): %s", tt.tz, err)
			}
			if tt.wantLoc == "" {
				if time.Local != orig {
					t.Errorf("空文字で time.Local が変更された: %v", time.Local)
				}
				return
			}
			if got := time.Local.String(); got != tt.wantLoc {
				t.Errorf("time.Local が合わない: want %s, got %s", tt.wantLoc, got)
			}
		})
	}
}

// Test_getMb はバイト数から MB への変換を確認する。
//
// 小数第 2 位までの切り捨てである。四捨五入に変えるとログの数値が変わる。
func Test_getMb(t *testing.T) {
	tests := []struct {
		name string
		size int64
		want float64
	}{
		{name: "0 バイト", size: 0, want: 0},
		{name: "1 MB", size: 1024 * 1024, want: 1},
		{name: "1.5 MB", size: 1024 * 1024 * 3 / 2, want: 1.5},
		{name: "1 KB は 0 に切り捨てられる", size: 1024, want: 0},
		// 切り捨てであることを固定する。0.999... は 0.99 になる。
		{name: "切り捨て", size: 1024*1024 - 1, want: 0.99},
		{name: "1 GB", size: 1024 * 1024 * 1024, want: 1024},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			if got := getMb(tt.size); got != tt.want {
				t.Errorf("getMb(%d) = %v, want %v", tt.size, got, tt.want)
			}
		})
	}
}

// Test_hitItem_JSON は export のフォーマットを固定する。
//
// このフォーマットは import のパーサと elasticsearch-dump との互換の
// 両方に関わる。フィールド名を変えると、既存のダンプファイルが読めなく
// なり、elasticsearch-dump との相互運用も壊れる。
func Test_hitItem_JSON(t *testing.T) {
	item := hitItem{
		Index:  "my_index",
		ID:     "163820696",
		Score:  1,
		Source: json.RawMessage(`{"id":163820696,"asset":""}`),
	}

	bs, err := json.Marshal(&item)
	if err != nil {
		t.Fatalf("marshal に失敗した: %s", err)
	}

	const want = `{"_index":"my_index","_id":"163820696","_score":1,"_source":{"id":163820696,"asset":""}}`
	if got := string(bs); got != want {
		t.Errorf("出力フォーマットが変わった:\n want %s\n got  %s", want, got)
	}

	// import 側が読み戻せることを確認する。export と import の対称性が
	// 崩れると、export したファイルを import できなくなる。
	var back hitItem
	if err := json.Unmarshal(bs, &back); err != nil {
		t.Fatalf("unmarshal に失敗した: %s", err)
	}
	if back.ID != item.ID {
		t.Errorf("_id が保たれない: want %s, got %s", item.ID, back.ID)
	}
	if back.Index != item.Index {
		t.Errorf("_index が保たれない: want %s, got %s", item.Index, back.Index)
	}
	if string(back.Source) != string(item.Source) {
		t.Errorf("_source が保たれない: want %s, got %s", item.Source, back.Source)
	}
}

// Test_isEmptyIndexSliceErr は空インデックスに固有の 400 だけを飲み込む
// ことを確認する。
//
// 判定が緩いと、マッピングの不整合や不正なクエリまで 0 件として扱われる。
// export が成功したように見えて 0 件しか出ないという最悪の壊れ方になる
// ため、判定条件を固定する。
func Test_isEmptyIndexSliceErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{
			name: "空インデックスの 400",
			err:  newElasticErr(400, "search_phase_execution_exception", "field _id not found"),
			want: true,
		},
		{
			// 同じ例外型でも理由が違えば飲み込まない。不正なクエリや
			// マッピングの問題は本来落とすべきエラーである。
			name: "同じ例外型だが別の理由",
			err:  newElasticErr(400, "search_phase_execution_exception", "failed to create query"),
			want: false,
		},
		{
			name: "index_not_found_exception",
			err:  newElasticErr(404, "index_not_found_exception", "no such index [foo]"),
			want: false,
		},
		{
			// 400 だが例外型が違う。ステータスだけで判定していないことを
			// 固定する。
			name: "400 で別の例外型",
			err:  newElasticErr(400, "parsing_exception", "field _id not found"),
			want: false,
		},
		{
			// Details が nil のレスポンスもある。参照する前に弾く必要が
			// ある。
			name: "Details が nil",
			err:  &elastic.Error{Status: 400},
			want: false,
		},
		{
			// RootCause が空でも Reason を走査して落ちてはいけない。
			name: "RootCause が空",
			err: &elastic.Error{
				Status: 400,
				Details: &elastic.ErrorDetails{
					Type:   "search_phase_execution_exception",
					Reason: "all shards failed",
				},
			},
			want: false,
		},
		{
			// elastic.Error 以外のエラーは飲み込まない。
			name: "elastic.Error ではない",
			err:  errors.New("field _id not found"),
			want: false,
		},
		{
			// ステータスが 400 でなければ別の問題である。
			name: "500 で同じ理由",
			err:  newElasticErr(500, "search_phase_execution_exception", "field _id not found"),
			want: false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			if got := isEmptyIndexSliceErr(tt.err); got != tt.want {
				t.Errorf("isEmptyIndexSliceErr(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// newElasticErr はテスト用に *elastic.Error を組み立てるヘルパである。
//
// elastic.Error はネストした構造体を持つ。テストケースごとに手で書くと
// 意図が読めなくなるため、必要な 3 つの値だけを受ける形にしている。
func newElasticErr(status int, typ, reason string) error {
	return &elastic.Error{
		Status: status,
		Details: &elastic.ErrorDetails{
			Type:   typ,
			Reason: reason,
			RootCause: []*elastic.ErrorDetails{
				{Type: typ, Reason: reason},
			},
		},
	}
}

// Test_assertLocalTestEndpoint はテストの接続先ガードを確認する。
//
// このガードは統合テストが本番のインデックスを削除する事故を防ぐ。
// seedIndex は DeleteIndex を呼ぶが、これは復旧不能な操作である。
// ESDUMP_TEST_ES に本番のエンドポイントが入ったまま go test を打つと
// そのクラスタのインデックスが消える。
//
// health チェックはガードにならない。到達できる場合に通すため、本番
// クラスタほど確実に通過してしまう。ホスト名で判定する必要がある。
//
// ガード自体が壊れると事故を防げなくなるため、テストで固定する。
func Test_assertLocalTestEndpoint(t *testing.T) {
	tests := []struct {
		name        string
		esUrl       string
		allowRemote string
		// wantFatal はテストが停止されることを期待するかである。
		wantFatal bool
	}{
		{name: "127.0.0.1", esUrl: "http://127.0.0.1:19217", wantFatal: false},
		{name: "localhost", esUrl: "http://localhost:19217", wantFatal: false},
		{name: "IPv6 ループバック", esUrl: "http://[::1]:19217", wantFatal: false},
		// ローカル以外は既定で弾く。
		{name: "本番風のホスト名", esUrl: "https://prod-es.internal:9200", wantFatal: true},
		{name: "IP アドレス直指定", esUrl: "http://10.0.0.5:9200", wantFatal: true},
		// 127.0.0.1 を含むが別のホストである文字列を通してはいけない。
		{name: "ホスト名に 127.0.0.1 を含む", esUrl: "http://127.0.0.1.example.com:9200", wantFatal: true},
		{name: "localhost を含むサブドメイン", esUrl: "http://localhost.example.com:9200", wantFatal: true},
		// 明示的な opt-in があれば通す。事故と意図を区別する。
		{name: "opt-in でローカル以外を許可", esUrl: "https://staging-es.internal:9200", allowRemote: "1", wantFatal: false},
		// 1 以外の値では通さない。true や yes を通すと、値の書き間違いが
		// 意図した opt-in として扱われる。
		{name: "opt-in の値が 1 以外", esUrl: "https://prod-es.internal:9200", allowRemote: "true", wantFatal: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ESDUMP_TEST_ALLOW_REMOTE", tt.allowRemote)

			if got := checkLocalTestEndpoint(tt.esUrl); (got != nil) != tt.wantFatal {
				t.Errorf("checkLocalTestEndpoint(%q) = %v, wantFatal %v", tt.esUrl, got, tt.wantFatal)
			}
		})
	}
}

// Test_esRequired は ESDUMP_TEST_ES_REQUIRED の値判定を確認する。
//
// 空でないことを条件にすると ESDUMP_TEST_ES_REQUIRED=0 が有効になり、
// 無効化したつもりの指定が有効として扱われる。ドキュメントも Makefile も
// =1 と書いているため、1 だけを受ける。
func Test_esRequired(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{value: "1", want: true},
		{value: "", want: false},
		{value: "0", want: false},
		{value: "true", want: false},
		{value: "yes", want: false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run("value="+tt.value, func(t *testing.T) {
			t.Setenv("ESDUMP_TEST_ES_REQUIRED", tt.value)
			if got := esRequired(); got != tt.want {
				t.Errorf("esRequired() = %v, want %v (値 %q)", got, tt.want, tt.value)
			}
		})
	}
}

// Test_testIndexPrefix は seedIndex がインデックス名の接頭辞を強制する
// ことを確認する。
//
// 接頭辞は接続先を間違えた場合の被害範囲を限定する。ガードが 1 段だけだと
// それが壊れたときに何も残らない。
func Test_testIndexPrefix(t *testing.T) {
	if !strings.HasPrefix("esdump_test_foo", testIndexPrefix) {
		t.Errorf("testIndexPrefix が %q になっている", testIndexPrefix)
	}
	// テストが使うインデックス名は必ずこの接頭辞で始まる。接頭辞を
	// 変えると seedIndex が全テストで停止するため、値を固定する。
	if testIndexPrefix != "esdump_test_" {
		t.Errorf("testIndexPrefix が変わった: %q", testIndexPrefix)
	}
}

// Test_truncateReason は拒否の理由が切り詰められることを確認する。
//
// ES の Reason にはドキュメントのフィールド値が埋め込まれ、ES 側では
// 切り詰められない。エラーは標準出力へ出るため、切り詰めないと業務データが
// CI のログや Issue に残る。
func Test_truncateReason(t *testing.T) {
	t.Run("上限以下はそのまま返す", func(t *testing.T) {
		reason := "failed to parse field [n]"
		if got := truncateReason(reason); got != reason {
			t.Errorf("切り詰めるべきでない: got %q", got)
		}
	})

	t.Run("上限ちょうどは切り詰めない", func(t *testing.T) {
		reason := strings.Repeat("a", maxReasonLen)
		if got := truncateReason(reason); got != reason {
			t.Errorf("上限ちょうどで切り詰めた: len %d", len([]rune(got)))
		}
	})

	t.Run("上限を超えたら切り詰めて印を付ける", func(t *testing.T) {
		reason := "type=x " + strings.Repeat("S", 5000)
		got := truncateReason(reason)
		if !strings.HasSuffix(got, "...(truncated)") {
			t.Errorf("切り詰めた印がない: %q", got[:60])
		}
		if n := len([]rune(strings.TrimSuffix(got, "...(truncated)"))); n != maxReasonLen {
			t.Errorf("切り詰めた長さが違う: got %d, want %d", n, maxReasonLen)
		}
		// 先頭の情報は残ること。type やフィールド名はここにある。
		if !strings.HasPrefix(got, "type=x ") {
			t.Errorf("先頭の情報が失われた: %q", got[:20])
		}
	})

	t.Run("マルチバイト文字を壊さない", func(t *testing.T) {
		reason := strings.Repeat("あ", 5000)
		got := truncateReason(reason)
		if !utf8.ValidString(got) {
			t.Error("不正なバイト列になった")
		}
		if n := len([]rune(strings.TrimSuffix(got, "...(truncated)"))); n != maxReasonLen {
			t.Errorf("文字数で切り詰めていない: got %d", n)
		}
	})
}

// Test_describeBulkFailure は拒否されたドキュメントの説明を確認する。
func Test_describeBulkFailure(t *testing.T) {
	t.Run("nil は unknown を返す", func(t *testing.T) {
		if got := describeBulkFailure(nil); got != "unknown" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("Error が nil でも id と status を出す", func(t *testing.T) {
		got := describeBulkFailure(&elastic.BulkResponseItem{Id: "a", Status: 400})
		if !strings.Contains(got, "_id=a") || !strings.Contains(got, "status=400") {
			t.Errorf("got %q", got)
		}
	})

	t.Run("長い理由は切り詰める", func(t *testing.T) {
		item := &elastic.BulkResponseItem{
			Id:     "a",
			Status: 400,
			Error: &elastic.ErrorDetails{
				Type:   "mapper_parsing_exception",
				Reason: strings.Repeat("S", 5000),
			},
		}
		got := describeBulkFailure(item)
		if strings.Count(got, "S") > maxReasonLen {
			t.Errorf("理由が切り詰められていない: %d 文字", strings.Count(got, "S"))
		}
		// 種別は残ること。原因の切り分けに要る。
		if !strings.Contains(got, "mapper_parsing_exception") {
			t.Errorf("type が失われた: %q", got)
		}
	})
}
