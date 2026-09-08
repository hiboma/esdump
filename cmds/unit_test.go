package cmds

import (
	"encoding/json"
	"testing"
	"time"

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
