package cmds

import (
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 一定バイト数を超えると容量不足を返す writer。ディスクフルを模す。
type limitedWriter struct {
	limit, written int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if w.written+len(p) > w.limit {
		allowed := w.limit - w.written
		if allowed < 0 {
			allowed = 0
		}
		w.written += allowed
		return allowed, errors.New("no space left on device")
	}
	w.written += len(p)
	return len(p), nil
}

// finish が書き込みの失敗を返すこと。
//
// これが本題である。bufio (4MB) に収まる量では、ループ中に一度も
// 書き込みが起きない。エラーは Flush と Close で初めて現れる。
// 戻り値を捨てる実装ではここが nil になり、末尾の欠けたファイルが
// exit 0 で残る。
//
// 検証は必ず newOutputWriter を呼んで行う。テスト内で closers を
// 組み立て直すと、実装ではなくテスト内のコピーを検証することになり、
// 実装を旧挙動に戻しても緑のままになる。
func TestFinishReportsWriteError(t *testing.T) {
	for _, useGzip := range []bool{false, true} {
		name := "plain"
		if useGzip {
			name = "gzip"
		}
		t.Run(name, func(t *testing.T) {
			sink := &limitedWriter{limit: 10}
			bw, finish := newOutputWriter(sink, nil, useGzip)

			// バッファに収まる量だけ書く。ここではまだ失敗しない。
			for i := 0; i < 100; i++ {
				if _, err := bw.Write([]byte(`{"_index":"i","_id":"x","_score":1,"_source":{}}`)); err != nil {
					t.Fatalf("バッファ内の書き込みで失敗した: %v", err)
				}
				if err := bw.WriteByte('\n'); err != nil {
					t.Fatalf("改行の書き込みで失敗した: %v", err)
				}
			}

			if err := finish(); err == nil {
				t.Fatal("書き込みが失敗しているのに finish が nil を返した。末尾の欠けたファイルが正常終了として残る")
			}
		})
	}
}

// finish が書き込み先の Close の失敗も返すこと。
func TestFinishReportsCloseError(t *testing.T) {
	closeErr := errors.New("close failed")
	sink := &limitedWriter{limit: 1 << 20}

	_, finish := newOutputWriter(sink, func() error { return closeErr }, false)

	if err := finish(); !errors.Is(err, closeErr) {
		t.Fatalf("Close の失敗が返らない: got %v, want %v", err, closeErr)
	}
}

// 確定処理を正しい順序で呼ぶこと。
//
// gzip を bufio より先に閉じると、bufio に残ったバイトが失われる。
// 全行が読み出せることで順序を確かめる。
func TestNewOutputWriterFlushesInOrder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.gz")

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		t.Fatal(err)
	}

	bw, finish := newOutputWriter(f, f.Close, true)

	const lines = 1000
	for i := 0; i < lines; i++ {
		if _, err := bw.WriteString(`{"_index":"i","_id":"x"}` + "\n"); err != nil {
			t.Fatal(err)
		}
	}

	if err := finish(); err != nil {
		t.Fatalf("finish が失敗した: %v", err)
	}

	rf, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()

	zr, err := gzip.NewReader(rf)
	if err != nil {
		t.Fatalf("gzip として読めない (終端が書かれていない): %v", err)
	}
	defer zr.Close()

	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gzip の読み出しで失敗した (末尾が欠けている): %v", err)
	}

	if got := strings.Count(string(body), "\n"); got != lines {
		t.Fatalf("行数が合わない: got %d, want %d", got, lines)
	}
}

// closeSink が nil なら書き込み先を閉じないこと。標準出力の経路である。
func TestNewOutputWriterSkipsNilCloser(t *testing.T) {
	closed := false
	sink := &limitedWriter{limit: 1 << 20}

	// nil を渡した場合は閉じない
	_, finish := newOutputWriter(sink, nil, false)
	if err := finish(); err != nil {
		t.Fatalf("finish が失敗した: %v", err)
	}
	if closed {
		t.Fatal("nil を渡したのに閉じられた")
	}

	// 渡した場合は閉じる
	_, finish2 := newOutputWriter(sink, func() error { closed = true; return nil }, false)
	if err := finish2(); err != nil {
		t.Fatalf("finish が失敗した: %v", err)
	}
	if !closed {
		t.Fatal("closeSink を渡したのに閉じられていない")
	}
}
