package cmds

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/olivere/elastic/v7"
	"github.com/spf13/cobra"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)
// Run ではなく RunE を使う。cobra は Run の戻り値を持たないため、
// エラーが呼び出し側に伝わらず終了コードが 0 のままになる。
//
// export が一部のスライスで失敗して全件揃っていない場合に成功と区別できない
// と、シェルや cron、CI から見て取りこぼしに気づけない。Execute() は
// エラーを受けて os.Exit(1) する。
var exportCmd = &cobra.Command{
	Use:   "export",
	Short: "elasticsearch export",
	Long:  `elasticsearch export`,
	// エラーは Execute() が 1 度出力するため、cobra 側の重複出力を止める。
	// usage も出さない。引数の誤りではなく実行時の失敗である。
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Printf("export index %s to %s",IndexName,Output)
		return ExportData(Output,EsUrl,IndexName,MatchBody)
	},
}

var importCmd = &cobra.Command{
	Use:   "import",
	Short: "elasticsearch import",
	Long:  `elasticsearch import`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Printf("import index %s from %s",IndexName,Input)
		return ImportData(Input,EsUrl,IndexName)
	},
}
var Output string
var MaxDocs int
var MatchBody string

var Input string
var enableGzip bool

// ProgressEvery は進捗ログを何件ごとに出すかである。0 で出力しない。
//
// 既定の 10000 件は 1 億件を超えるインデックスで 1 万行以上になり、
// logrotate の世代を進捗ログだけで埋める。その中に埋もれるとエラーや
// 転送完了の行を追えなくなるため、呼び出し側で間隔を選べるようにする。
var ProgressEvery int

// PagesEvery はフェッチ時間のログを何ページごとに出すかである。0 で出力しない。
var PagesEvery int

// PageSize は scroll の 1 ページあたりの取得件数である。
//
// 終了判定がこの値との比較で行われるため、定数ではなく変数として持つ。
// ハードコードした 100 と突き合わせると、値を変えたときに最終ページを
// 検出できず、無限ループか取りこぼしになる。
var PageSize int

// Slices は scroll を何本に分割して並行に読むかである。1 で分割しない。
//
// OpenSearch の sliced scroll を使う。1 つのインデックスを N 個の互いに
// 素な部分集合に分け、それぞれを独立した scroll として同時に読める。
// 単一 scroll は前のページが返るまで次を要求できないため、ここが
// 取得速度の上限になっている。
//
// 分割数はシャード数に合わせるのが定石である。シャード数より多くすると
// 1 シャードを複数のスライスが読むことになり、OpenSearch 側の負荷だけが
// 増える。
var Slices int

func init(){
	exportCmd.Flags().StringVarP(&Output,"o","o","./tmp_export.json.gz","export dest filename; use - for stdout")
	exportCmd.Flags().IntVarP(&MaxDocs,"c","c",0,"set the max amount of documents to be exported; default(0) will exported all matched document; ")
	exportCmd.Flags().StringVarP(&MatchBody,"MatchBody","m","{\"match_all\":{}}","MatchBody, empty for match_all; example:{\"range\": {\"timestamp\": {\"gte\": \"2021-04-20\"}}}")
	exportCmd.Flags().BoolVar(&enableGzip,"gzip",true,"enable gzip; to disable gzip add parameter \"--gzip=false\"")
	exportCmd.Flags().IntVar(&ProgressEvery,"progress-every",10000,"log export progress every N documents; 0 disables progress logging")
	exportCmd.Flags().IntVar(&PagesEvery,"pages-every",1000,"log fetch time every N pages; 0 disables fetch time logging")
	exportCmd.Flags().IntVar(&PageSize,"size",100,"scroll page size; number of documents fetched per request")
	exportCmd.Flags().IntVar(&Slices,"slices",1,"number of sliced scrolls to read in parallel; 1 disables slicing; match the index shard count")

	importCmd.Flags().StringVarP(&Input,"i","i","./tmp_import.json.gz","import filename; use - for stdin")
	importCmd.Flags().BoolVar(&enableGzip,"gzip",true,"enable gzip; to disable gzip add parameter \"--gzip=false\"")
	//importCmd.MarkFlagRequired("i")
	//exportCmd.MarkFlagRequired("o")

	RootCmd.AddCommand(exportCmd)
	RootCmd.AddCommand(importCmd)
}

func ImportData(inputFile ,esUrl,indexName string)(err error){
	var inFile *os.File
	if inputFile=="-"{
		inFile =os.Stdin
	}else{
		inFile,err=os.Open(inputFile)
	}
	// オープン失敗はエラーとして返す。log.Fatalf は os.Exit を呼ぶため
	// defer した inFile.Close() が実行されず、RunE がエラーを受け取る
	// 経路も通らない。終了コードは 1 になるが、扱いが export 側と揃わない。
	if err != nil {
		return fmt.Errorf("open inputFile %s: %w", inputFile, err)
	}
	defer inFile.Close()

	var sourceReader io.Reader
	if enableGzip{
		zipReader,err1 := gzip.NewReader(inFile)
		if err1 != nil {
			// err1 を返す。名前付き戻り値の err はオープンが成功した時点で
			// nil であり、これを返すと非 gzip ファイルを渡しても 0 件を
			// import して正常終了する。export | import のパイプラインで
			// 壊れたダンプと成功を区別できなくなる。
			if errors.Is(err1,gzip.ErrHeader) {
				log.Println("the input file is not gzipped format", err1)
			}
			return err1
		}
		sourceReader=zipReader
		defer zipReader.Close()
		log.Println("import gziped file")
	}else{
		sourceReader= inFile
		log.Println("import none gziped file")
	}

	bufReader:=bufio.NewReaderSize(sourceReader,1<<22)
	iserv:=GetEsIndexService(esUrl,indexName)
	counter:=0;
	// failed は ES に拒否されたドキュメントの数である。
	//
	// bulk はリクエスト全体が成功しても、個々のドキュメントが拒否される。
	// マッピング不一致 (mapper_parsing_exception) が典型で、レスポンスの
	// items それぞれに error が入る。Do() の戻り値のエラーは HTTP や接続の
	// 失敗しか表さないため、これを見るだけでは 1 件も入らなくても成功に見える。
	failed := 0
	// firstFailure は最初に拒否された理由である。件数だけでは原因が分からず、
	// かといって全件分の理由を出すとログが溢れるため、1 件だけ残す。
	var firstFailure string
	for line, _, err := bufReader.ReadLine(); err != io.EOF; line, _, err = bufReader.ReadLine() {
		counter++
		item:=new(hitItem)
		err=json.Unmarshal(line,item)
		if err != nil {
			return fmt.Errorf("line %d の JSON を解釈できない: %w", counter, err)
		}
		//提交数据
		req:= elastic.NewBulkIndexRequest()
		req.Id(item.ID).Doc(item.Source)
		iserv.Add(req)
		if iserv.NumberOfActions()>999{
			if berr := flushImportBulk(iserv, &failed, &firstFailure); berr != nil {
				return berr
			}
			log.Printf("row count %d",counter)
		}
	}

	//LAST:
	if iserv.NumberOfActions()>0{
		if berr := flushImportBulk(iserv, &failed, &firstFailure); berr != nil {
			return berr
		}
	}
	log.Printf("finish import row count %d",counter)

	// 拒否されたドキュメントがあれば失敗として返す。
	//
	// 以前はここまで到達すれば常に nil を返していた。全件が拒否されても
	// "finish import row count N" を出して exit 0 で終わるため、cron や CI
	// から成功と区別できなかった。件数を報告している分、成功したように読める。
	if failed > 0 {
		return fmt.Errorf("%d / %d 件の import が ES に拒否された: %s", failed, counter, firstFailure)
	}
	return
}

// flushImportBulk は溜まったリクエストを送り、拒否されたドキュメントを数える。
//
// 戻り値のエラーはリクエスト自体の失敗 (接続断や HTTP エラー) である。
// この場合は以降も失敗し続けるため、呼び出し側は中断する。個々の
// ドキュメントの拒否は failed に加算するだけで続行する。ダンプの一部に
// 不正な行が混ざっていても、残りを入れ切るほうが実用的である。
func flushImportBulk(iserv *elastic.BulkService, failed *int, firstFailure *string) error {
	res, err := iserv.Do(context.Background())
	if err != nil {
		return fmt.Errorf("bulk request に失敗した: %w", err)
	}
	for _, item := range res.Failed() {
		*failed++
		if *firstFailure == "" {
			*firstFailure = describeBulkFailure(item)
		}
	}
	return nil
}

// describeBulkFailure は拒否されたドキュメント 1 件の理由を 1 行にする。
func describeBulkFailure(item *elastic.BulkResponseItem) string {
	if item == nil {
		return "unknown"
	}
	if item.Error == nil {
		return fmt.Sprintf("_id=%s status=%d", item.Id, item.Status)
	}
	return fmt.Sprintf("_id=%s status=%d type=%s reason=%s", item.Id, item.Status, item.Error.Type, item.Error.Reason)
}
// fetchSlice は 1 本の scroll を読み切り、ヒットを dataChan へ送る。
//
// slices が 1 のときは分割せず、インデックス全体を 1 本の scroll で読む。
// 2 以上のときは sliced scroll を使い、sliceID 番目の部分集合だけを読む。
// 各スライスは互いに素な集合を返すため、全スライスの和が全件になる。
//
// scroll は前のレスポンスが返す scroll ID がないと次を要求できず、本質的に
// 逐次である。並行化はスライスを分ける以外に方法がない。
//
// count と totalFetchTime は全スライスで共有する。atomic で更新すること。
//
// エラーは戻り値で返す。以前はここで log.Fatalln していたが、os.Exit は
// ExportData の defer を実行しない。gzip writer の Close とバッファの Flush が
// 飛ぶため、それまでに取得したデータが失われ、出力ファイルは途中で切れた
// 不完全な gzip ストリームになる。数時間かけた export の終盤で 1 度エラーが
// 出ただけで成果物全体が使えなくなる。
func fetchSlice(esUrl, indexName, matchBody string, pageSize, sliceID, slices int,
	dataChan chan<- interface{}, count *int64, totalFetchTime *int64) error {

	ss := GetEsScrollService(esUrl, indexName)
	if matchBody != "" {
		rawQuery := elastic.NewRawStringQuery(matchBody)
		ss = ss.Query(rawQuery)
		if sliceID == 0 {
			log.Println("export match:", matchBody)
		}
	}
	if slices > 1 {
		ss = ss.Slice(elastic.NewSliceQuery().Id(sliceID).Max(slices))
	}
	pager := ss.Size(pageSize)

	pcounter := 0
	fetchTime := 0.0

	for {
		startTime := time.Now()
		res, err := pager.Do(context.Background())
		spend := time.Now().Sub(startTime).Seconds()
		fetchTime += spend
		atomic.AddInt64(totalFetchTime, int64(time.Duration(spend*float64(time.Second))))
		pcounter++
		// 0 は無効を意味する。剰余は 0 で panic するため必ず先に弾く。
		if PagesEvery > 0 && pcounter%PagesEvery == 0 {
			if slices > 1 {
				log.Printf("slice %d: %d pages FetchTime %v s", sliceID, PagesEvery, fetchTime)
			} else {
				log.Printf("%d pages FetchTime %v s", PagesEvery, fetchTime)
			}
			fetchTime = 0
		}
		if err == nil {
			for _, hit := range res.Hits.Hits {
				dataChan <- *hit
				// MaxDocs は全スライス合計で判定する。スライスごとに数えると
				// slices 倍の件数を取得してしまう。
				n := atomic.AddInt64(count, 1)
				if MaxDocs > 0 && n >= int64(MaxDocs) {
					return nil
				}
			}
			// 最終ページはヒット数が Size 未満になる。
			if len(res.Hits.Hits) < pageSize {
				return nil
			}
		}
		// io.EOF は scroll を読み切ったことを示す正常終了の合図であり、
		// エラーではない。olivere/elastic の ScrollService.Do は
		// ヒットが 0 件になった時点で io.EOF を返す。
		//
		// 最終ページのヒット数が Size 未満であれば上の分岐で抜ける。
		// しかし総件数が Size の倍数ちょうどのときは最終ページが Size と
		// 同数になり、次の Do が 0 件 + io.EOF を返す。ここを異常終了として
		// 扱うと、全件を読み終えているのにプロセスが落ちて出力が失われる。
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			// 一度もドキュメントが入っていないインデックスに sliced scroll を
			// 投げると 400 になる。スライスの分割は既定で _id のフィールド
			// データを使うが、空のセグメントには _id が存在せず OpenSearch が
			// "field _id not found" を返す。空を読んだ結果が 0 件であることは
			// 変わらないため、正常終了として扱う。
			//
			// ドキュメントを入れて全件削除した後のインデックスでは発生しない。
			// セグメントに _id が残るためである。発生するのは新規作成して
			// 一度も書き込んでいない場合だけである。
			if isEmptyIndexSliceErr(err) {
				log.Printf("slice %d: インデックス %s は空のため 0 件で終了する", sliceID, indexName)
				return nil
			}
			return fmt.Errorf("ScrollService err (slice %d): %w", sliceID, err)
		}
	}
}

// isEmptyIndexSliceErr は空インデックスへの sliced scroll に固有の 400 か
// どうかを判定する。
//
// 判定を "field _id not found" の有無まで絞る。search_phase_execution_exception
// だけで判定すると、マッピングの不整合や不正なクエリなど本来落とすべき
// エラーまで 0 件として飲み込み、取りこぼしに気づけなくなる。
func isEmptyIndexSliceErr(err error) bool {
	var esErr *elastic.Error
	if !errors.As(err, &esErr) {
		return false
	}
	if esErr.Status != http.StatusBadRequest || esErr.Details == nil {
		return false
	}
	if esErr.Details.Type != "search_phase_execution_exception" {
		return false
	}
	for _, rc := range esErr.Details.RootCause {
		if rc != nil && strings.Contains(rc.Reason, "field _id not found") {
			return true
		}
	}
	return false
}

// newOutputWriter は出力先とその確定処理を組み立てる。
//
// 返す finish は、積んだ Flush と Close を上流から下流の順に呼び、最初の
// エラーを返す。呼び出し側は必ず戻り値を確認すること。
//
// bufio と gzip はバッファに溜めるため、最後の Flush と Close で初めて
// 書き込まれる分がある。gzip は Close で残りをフラッシュして終端を書く。
// ここで戻り値を捨てると、末尾の欠けたファイルが exit 0 で残る。
//
// 以前は defer で Flush と Close を並べ、戻り値を全て捨てていた。バッファ
// (4MB) に収まる量ならループ内で一度も書き込まれないため、書き込み先が
// 容量不足でも err は nil のままだった。呼び出し側は成功と判断していた。
//
// sink は書き込み先である。テストから任意の writer を渡せるよう *os.File
// ではなくインターフェースで受ける。closeSink が nil なら書き込み先は
// 閉じない (標準出力の場合)。
func newOutputWriter(sink io.Writer, closeSink func() error, useGzip bool) (*bufio.Writer, func() error) {
	targetWriter := sink
	var closers []func() error

	if useGzip {
		zip := gzip.NewWriter(sink)
		targetWriter = zip
		closers = append(closers, zip.Close)
	}

	outputWriter := bufio.NewWriterSize(targetWriter, 1<<22)
	// bufio を先に流す。gzip より後にすると、まだ渡していないバイトを捨てる。
	closers = append([]func() error{outputWriter.Flush}, closers...)

	if closeSink != nil {
		closers = append(closers, closeSink)
	}

	finish := func() error {
		var firstErr error
		for _, closeFn := range closers {
			// 途中で失敗しても残りを呼ぶ。ファイルディスクリプタを
			// 漏らさないためである。
			if cerr := closeFn(); cerr != nil && firstErr == nil {
				firstErr = cerr
			}
		}
		return firstErr
	}
	return outputWriter, finish
}

func ExportData(outputFile ,esUrl,indexName,matchBody string)(err error) {
	var ofile *os.File
	isStdout := outputFile == "-"
	if isStdout{
		ofile=os.Stdout
	}else{
		if enableGzip && !strings.HasSuffix(outputFile,".gz"){
			outputFile+=".gz"
		}
		ofile, err= os.OpenFile(outputFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	}

	// オープンの成否を先に見る。失敗時は ofile が nil であり、確定処理を
	// 組み立てる前に返さなければ nil ポインタを参照する。
	if err != nil {
		log.Print("open file err", err)
		return err
	}

	// サマリは確定処理より先に defer へ積む。defer は LIFO なので、
	// 最初に積んだこれが最後に走る。
	//
	// gzip のサイズはパスで Stat して読む。4MB の bufio バッファと gzip の
	// 内部バッファは finish() の Flush と Close で初めてファイルへ届くため、
	// それより前に読むとバッファに残った分を数えず、実際のファイルより
	// 小さい値を報告する。30 万件で 2.44 MB と 2.63 MB の差が出る。
	//
	// ofile.Stat() は使えない。この時点で閉じられており file already closed
	// になる。
	//
	// storeCount、bsCounter、storeStart はクロージャが参照する。宣言は
	// この後だが、実行時には確定している。
	//
	// storeStart は最後に進捗ログを出した時刻である。ゼロ値のままサマリを
	// 出すと 1970 年からの経過秒になるため、受信ループの直前で初期化する。
	var storeCount, bsCounter int
	var storeStart time.Time
	defer func() {
		// storeStart がゼロ値なら受信ループに入る前に抜けている。
		// 計測していない値を出さない。
		storeTime := 0.0
		if !storeStart.IsZero() {
			storeTime = time.Since(storeStart).Seconds()
		}
		// 標準出力ではサイズを報告しない。パイプや端末の Stat は
		// 書き出したバイト数を返さない。
		if !enableGzip || isStdout {
			log.Printf("total exported %d items; total_raw_bytes: %.2f MB; storeTime %f", storeCount, getMb(int64(bsCounter)), storeTime)
			return
		}
		stat, e := os.Stat(outputFile)
		if e != nil {
			log.Printf("total exported %d items; total_raw_bytes: %.2f MB; gzip size の取得に失敗した: %s", storeCount, getMb(int64(bsCounter)), e)
			return
		}
		log.Printf("total exported %d items; total_raw_bytes: %.2f MB;the gzip size: %.2f MB", storeCount, getMb(int64(bsCounter)), getMb(stat.Size()))
	}()

	// 標準出力は閉じない。以降のログ出力を妨げる。
	var closeSink func() error
	if !isStdout {
		closeSink = ofile.Close
	}
	outputWriter, finish := newOutputWriter(ofile, closeSink, enableGzip)

	// 書き込みの確定でエラーを捨てない。
	//
	// 名前付き戻り値の err に集約する。既にエラーがあればそれを優先し、
	// 上書きしない。最初の失敗こそが原因に近い。
	defer func() {
		if cerr := finish(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	pageSize := PageSize
	if pageSize <= 0 {
		pageSize = 100
	}
	slices := Slices
	if slices <= 0 {
		slices = 1
	}

	dataChan :=make(chan interface{},300)

	// count は全スライス合計の取得件数である。MaxDocs の判定に使うため
	// atomic で更新する。スライスごとの独立したカウンタでは、合計が
	// MaxDocs を超えてから止まることになる。
	var count int64
	var totalFetchTime int64 // ナノ秒。複数のスライスから加算する

	// fetchErrs は各スライスのエラーを集める。1 本でも失敗したら export は
	// 全件を出力できていない。ExportData の戻り値に反映して呼び出し側が
	// 気づけるようにする。以前は log.Fatalln で即死していたため、失敗を
	// 判別する手段が終了コードしかなく、書き出し中のデータも失われていた。
	fetchErrs := make([]error, slices)

	var wg sync.WaitGroup
	for i := 0; i < slices; i++ {
		wg.Add(1)
		go func(sliceID int) {
			defer wg.Done()
			fetchErrs[sliceID] = fetchSlice(esUrl, indexName, matchBody, pageSize, sliceID, slices, dataChan, &count, &totalFetchTime)
		}(i)
	}

	go func() {
		wg.Wait()
		close(dataChan)
		log.Println("totalFetchTime", time.Duration(atomic.LoadInt64(&totalFetchTime)).Seconds(), "s")
	}()

	// writeErr は書き出し側のエラーである。err に直接入れず分けているのは、
	// 最後に fetch 側のエラーと合わせて判定するためである。
	var writeErr error

	// storeTime は出力側 (JSON 化と書き込み) にかけた時間である。
	//
	// 以前は 0 で初期化してログに出すだけで、測定値を代入していなかった。
	// ログの storeTime は常に 0.000000 だった。取得待ち (totalFetchTime) と
	// 突き合わせて、どちらがボトルネックかを判断するための値なので、
	// 測っていなければ意味がない。
	//
	// 区間の始点だけを持ち、ログを出すたびに測り直す。毎件 time.Now() を
	// 呼ぶと実測で 7.2% 遅くなった。計測のために遅くするのは本末転倒である。
	// チャネル待ちを含む値になるが、出力側が詰まっているかを見るには足りる。
	//
	// 変数はサマリの defer が参照するため上で宣言している。ここで代入する。
	storeStart = time.Now()
	for  chanItem := range dataChan {
		// 書き出しに失敗した後もチャネルは読み切る。
		//
		// ここで break すると受信側が消える。fetchSlice は dataChan への
		// 送信でブロックしたまま止まり、wg.Wait() が返らないため close も
		// 行われず、プロセスがハングする。バッファは 300 しかないため、
		// 大きなインデックスでは確実にこの状態になる。
		if writeErr != nil {
			continue
		}
		storeCount +=1
		hit:=chanItem.(elastic.SearchHit)
		item:=hitItem{hit.Index, hit.Id, 1, hit.Source}
		// Marshal のエラーを捨てない。捨てると空の bs が書かれ、
		// 壊れた行が出力に混ざったまま正常終了する。
		//
		// break ではなく writeErr へ入れて読み続ける。break すると
		// fetchSlice が dataChan への送信でブロックし、プロセスがハングする。
		bs,merr:=json.Marshal(&item)
		if merr != nil {
			writeErr = fmt.Errorf("json marshal err (index=%s id=%s): %w", hit.Index, hit.Id, merr)
			log.Println(writeErr)
			continue
		}
		// 1 本目の戻り値を 2 本目で上書きしない。bufio はエラーを保持する
		// ため実害は出にくいが、どちらで失敗したか分からなくなる。
		if _, werr := outputWriter.Write(bs); werr != nil {
			log.Println("io err:", werr)
			writeErr = werr
			continue
		}
		if werr := outputWriter.WriteByte('\n'); werr != nil {
			log.Println("io err:", werr)
			writeErr = werr
			continue
		}
		bsCounter+=len(bs)
		// 0 は無効を意味する。剰余は 0 で panic するため必ず先に弾く。
		if ProgressEvery > 0 && storeCount%ProgressEvery==0{
			log.Printf("total exported %d items; total_raw_bytes: %.2f MB; storeTime %f", storeCount, getMb(int64(bsCounter)), time.Since(storeStart).Seconds())
			storeStart = time.Now()
		}
	}

	// fetch 側のエラーを集約する。書き出しは成功していても全件揃っていない
	// ため、成功として返してはいけない。
	err = errors.Join(append([]error{writeErr}, fetchErrs...)...)
	if err != nil {
		log.Print(err)
	}
	return err
}
func getMb(size int64) float64{
	tmpf:=float64(size)/(1024*1024)*100
	tmpf=math.Trunc(tmpf)/100
	return tmpf
}
type hitItem struct {
	Index  string          `json:"_index"`
	ID     string          `json:"_id"`
	Score  int             `json:"_score"`
	Source json.RawMessage `json:"_source"`
}