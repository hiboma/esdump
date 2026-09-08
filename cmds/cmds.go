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
	for line, _, err := bufReader.ReadLine(); err != io.EOF; line, _, err = bufReader.ReadLine() {
		counter++
		item:=new(hitItem)
		err=json.Unmarshal(line,item)
		if err != nil {
			return err
		}
		//提交数据
		req:= elastic.NewBulkIndexRequest()
		req.Id(item.ID).Doc(item.Source)
		iserv.Add(req)
		if iserv.NumberOfActions()>999{
			_,err=iserv.Do(context.Background())
			if err != nil {
				log.Println(err)
				err=nil
			}
			log.Printf("row count %d",counter)
		}
		if err != nil {
			log.Println(err)
		}
	}

	//LAST:
	if iserv.NumberOfActions()>0{
		_,err=iserv.Do(context.Background())
		if err != nil {
			log.Println("es err", err)
		}
	}
	log.Printf("finish import row count %d",counter)
	return
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

	// オープンの成否を先に見る。失敗時は ofile が nil であり、defer に
	// Close を積む前に返さなければ nil ポインタを参照する。
	if err != nil {
		log.Print("open file err", err)
		return err
	}

	// 遅延実行のエラーは名前付き戻り値へ書き戻す。
	//
	// 4MB の bufio バッファと gzip の内部バッファに残ったデータは、最後の
	// Flush と Close で初めてファイルへ届く。ディスクが満杯 (ENOSPC) に
	// なるとここで初めて失敗するため、書き出し中は成功していたことを理由に
	// 戻り値が nil のままになり、gzip のフッタ (CRC32 と ISIZE) を欠いた
	// 出力を成功として返す。
	//
	// なお -o - でのパイプ切断はここには来ない。Go の runtime は fd 1 と 2
	// の SIGPIPE を EPIPE に変換せず再送するため、プロセスが signal で死ぬ。
	// 読み手が消えている以上、拾っても回復できるものはない。
	//
	// 呼び出し側は成果物が使えないことに気づけないため、必ず拾う。
	// 先に入ったエラーを優先する。原因に近いのは最初のエラーである。
	keepErr := func(e error) {
		if e != nil && err == nil {
			err = e
		}
	}

	// サマリは Flush と Close より先に defer へ積む。defer は LIFO なので、
	// 最初に積んだこれが最後に走る。
	//
	// gzip のサイズは ofile.Stat() で読む。4MB の bufio バッファと gzip の
	// 内部バッファは Flush と Close で初めてファイルへ届くため、それより
	// 前に Stat するとバッファに残った分を数えず、実際のファイルより
	// 小さい値を報告する。30 万件で 2.44 MB と 2.63 MB の差が出る。
	//
	// storeCount と bsCounter はクロージャが参照する。宣言はこの後だが、
	// 実行時には確定している。
	var storeCount, bsCounter int
	storeTime := 0.0
	defer func() {
		// 標準出力ではサイズを報告しない。パイプや端末の Stat は
		// 書き出したバイト数を返さない。
		if !enableGzip || isStdout {
			log.Printf("total exported %d items; total_raw_bytes: %.2f MB; storeTime %f", storeCount, getMb(int64(bsCounter)), storeTime)
			return
		}
		// ofile.Stat() ではなくパスで Stat する。ofile はこの時点で
		// 閉じられており、file already closed になる。
		stat, e := os.Stat(outputFile)
		if e != nil {
			log.Printf("total exported %d items; total_raw_bytes: %.2f MB; gzip size の取得に失敗した: %s", storeCount, getMb(int64(bsCounter)), e)
			return
		}
		log.Printf("total exported %d items; total_raw_bytes: %.2f MB;the gzip size: %.2f MB", storeCount, getMb(int64(bsCounter)), getMb(stat.Size()))
	}()

	// 標準出力は閉じない。閉じると後続の書き込みが失敗する。
	if !isStdout {
		defer func() { keepErr(ofile.Close()) }()
	}

	var targetWriter io.Writer
	if enableGzip{
		zip := gzip.NewWriter(ofile)
		// gzip の Close はフッタを書く。Flush だけでは gzip ストリームが
		// 完成しない。defer は LIFO で、bufio の Flush より後に走る。
		defer func() { keepErr(zip.Close()) }()
		targetWriter=zip
	}else{
		targetWriter=ofile
	}

	outputWriter:=bufio.NewWriterSize(targetWriter,1<<22)
	defer func() { keepErr(outputWriter.Flush()) }()
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
		bs,_:=json.Marshal(&item)
		if _, e := outputWriter.Write(bs); e != nil {
			log.Println("io err:", e)
			writeErr = e
			continue
		}
		if _, e := outputWriter.Write([]byte("\n")); e != nil {
			log.Println("io err:", e)
			writeErr = e
			continue
		}
		bsCounter+=len(bs)
		// 0 は無効を意味する。剰余は 0 で panic するため必ず先に弾く。
		if ProgressEvery > 0 && storeCount%ProgressEvery==0{
			log.Printf("total exported %d items; total_raw_bytes: %.2f MB; storeTime %f", storeCount, getMb(int64(bsCounter)), storeTime)
			storeTime =0
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