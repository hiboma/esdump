package cmds

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"github.com/olivere/elastic/v7"
	"github.com/spf13/cobra"
	"io"
	"log"
	"math"
	"os"
	"strings"
	"time"
)
var exportCmd = &cobra.Command{
	Use:   "export",
	Short: "elasticsearch export",
	Long:  `elasticsearch export`,
	Run: func(cmd *cobra.Command, args []string) {
		log.Printf("export index %s to %s",IndexName,Output)
		ExportData(Output,EsUrl,IndexName,MatchBody)
	},
}

var importCmd = &cobra.Command{
	Use:   "import",
	Short: "elasticsearch import",
	Long:  `elasticsearch import`,
	Run: func(cmd *cobra.Command, args []string) {
		log.Printf("import index %s from %s",IndexName,Input)
		err:=ImportData(Input,EsUrl,IndexName)
		if err != nil {
			log.Println(err)
		}
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

func init(){
	exportCmd.Flags().StringVarP(&Output,"o","o","./tmp_export.json.gz","export dest filename; use - for stdout")
	exportCmd.Flags().IntVarP(&MaxDocs,"c","c",0,"set the max amount of documents to be exported; default(0) will exported all matched document; ")
	exportCmd.Flags().StringVarP(&MatchBody,"MatchBody","m","{\"match_all\":{}}","MatchBody, empty for match_all; example:{\"range\": {\"timestamp\": {\"gte\": \"2021-04-20\"}}}")
	exportCmd.Flags().BoolVar(&enableGzip,"gzip",true,"enable gzip; to disable gzip add parameter \"--gzip=false\"")
	exportCmd.Flags().IntVar(&ProgressEvery,"progress-every",10000,"log export progress every N documents; 0 disables progress logging")
	exportCmd.Flags().IntVar(&PagesEvery,"pages-every",1000,"log fetch time every N pages; 0 disables fetch time logging")

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
	if err != nil {
		log.Fatalf("open inputFile %s with err: %s",inputFile,err)
	}
	defer inFile.Close()

	var sourceReader io.Reader
	if enableGzip{
		zipReader,err1 := gzip.NewReader(inFile)
		if err1 != nil {
			if errors.Is(err1,gzip.ErrHeader) {
				log.Println("the input file is not gzipped format", err)
			}
			return err
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
func ExportData(outputFile ,esUrl,indexName,matchBody string)(err error) {
	var ofile *os.File
	if outputFile=="-"{
		ofile=os.Stdout
	}else{
		if enableGzip && !strings.HasSuffix(outputFile,".gz"){
			outputFile+=".gz"
		}
		ofile, err= os.OpenFile(outputFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	}
	defer ofile.Close()

	if err != nil {
		log.Print("open file err", err)
		return err
	}
	var targetWriter io.Writer
	if enableGzip{
		zip := gzip.NewWriter(ofile)
		defer zip.Flush()
		defer zip.Close()
		targetWriter=zip
	}else{
		targetWriter=ofile
	}

	outputWriter:=bufio.NewWriterSize(targetWriter,1<<22)
	defer outputWriter.Flush()
	ss:=GetEsScrollService(esUrl,indexName)
	if matchBody!=""{
		rawQuery:=elastic.NewRawStringQuery(matchBody)
		ss=ss.Query(rawQuery)
		log.Println("export match:",matchBody)
	}
	pager:=ss.Size(100)//.Query(elastic.MatchAllQuery{})
	pcounter := 0
	count :=0
	dataChan :=make(chan interface{},300)
	fetchTime:=0.0
	totalFetchTime:=0.0
	go func() {
		defer close(dataChan)
		for {
			//for test
			startTime := time.Now()
			res, err := pager.Do(context.Background())
			spend:=time.Now().Sub(startTime).Seconds()
			fetchTime += spend
			totalFetchTime+=spend
			pcounter++;
			// 0 は無効を意味する。剰余は 0 で panic するため必ず先に弾く。
			if PagesEvery > 0 && pcounter % PagesEvery ==0 {
				log.Printf("%d pages FetchTime %v s", PagesEvery, fetchTime)
				fetchTime=0
			}
			if err == nil {
				for _, hit := range res.Hits.Hits {
					dataChan <- *hit
					count++
					if MaxDocs > 0 && count >= MaxDocs {
						goto END
					}
				}
				if len(res.Hits.Hits) < 100 {
					goto END
				}
			}
			// io.EOF は scroll を読み切ったことを示す正常終了の合図であり、
			// エラーではない。olivere/elastic の ScrollService.Do は
			// ヒットが 0 件になった時点で io.EOF を返す。
			//
			// 最終ページのヒット数が Size 未満であれば上の分岐で END へ抜ける。
			// しかし総件数が Size の倍数ちょうどのときは最終ページが Size と
			// 同数になり、次の Do が 0 件 + io.EOF を返す。ここを異常終了として
			// 扱うと、全件を読み終えているのにプロセスが落ちて出力が失われる。
			if errors.Is(err, io.EOF) {
				goto END
			}
			if err != nil {
				log.Fatalln("ScrollService err", err)
			}
		}
	END:
		log.Println("totalFetchTime", totalFetchTime, "s")
	}()

	storeTime :=0.0
	bsCounter:=0
	storeCount :=0
	for  chanItem := range dataChan {
		storeCount +=1
		hit:=chanItem.(elastic.SearchHit)
		item:=hitItem{hit.Index, hit.Id, 1, hit.Source}
		bs,_:=json.Marshal(&item)
		_, err = outputWriter.Write(bs)
		_, err = outputWriter.Write([]byte("\n"))
		if err != nil {
			log.Println("io err:",err)
			break
		}
		bsCounter+=len(bs)
		// 0 は無効を意味する。剰余は 0 で panic するため必ず先に弾く。
		if ProgressEvery > 0 && storeCount%ProgressEvery==0{
			log.Printf("total exported %d items; total_raw_bytes: %.2f MB; storeTime %f", storeCount, getMb(int64(bsCounter)), storeTime)
			storeTime =0
		}
	}
	if err != nil {
		log.Print(err)
	}
	if  enableGzip {
		stat, _ := ofile.Stat()
		fsize := getMb(stat.Size())
		log.Printf("total exported %d items; total_raw_bytes: %.2f MB;the gzip size: %.2f MB", storeCount, getMb(int64(bsCounter)), fsize)
	}else{
		log.Printf("total exported %d items; total_raw_bytes: %.2f MB; storeTime %f", storeCount, getMb(int64(bsCounter)), storeTime)
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