package main

import (
	"os/exec"
	"strings"
	"testing"
)

// Test_Build はパッケージがビルドできることを確認する。
//
// 統合テストは cmds パッケージ側にある。main パッケージは cmds.Execute() を
// 呼ぶだけであり、ここで検証するものは配線が通っていることだけである。
//
// 以前ここにあった Test_Gzip は先頭で return しており何も検証していなかった。
// Test_Export は到達できないホスト (http://brige:9200) を参照していた。
// どちらも失敗を検出できないため、実際に検証する内容へ置き換えた。
func Test_Build(t *testing.T) {
	if testing.Short() {
		t.Skip("go build を実行するため -short ではスキップする")
	}
	cmd := exec.Command("go", "build", "-o", t.TempDir()+"/esdump", ".")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build に失敗した: %s\n%s", err, out)
	}
}

// Test_Help はサブコマンドが登録されていることを確認する。
//
// cobra へのコマンド登録は init() で行われる。登録が漏れるとフラグの
// 追加時などに気づかないまま export や import が消える。
func Test_Help(t *testing.T) {
	if testing.Short() {
		t.Skip("バイナリを実行するため -short ではスキップする")
	}
	bin := t.TempDir() + "/esdump"
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build に失敗した: %s\n%s", err, out)
	}

	out, err := exec.Command(bin, "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("--help の実行に失敗した: %s\n%s", err, out)
	}
	for _, want := range []string{"export", "import", "version"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("--help に %q が含まれない:\n%s", want, out)
		}
	}
}
