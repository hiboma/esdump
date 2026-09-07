// Copyright © 2018 NAME HERE <EMAIL ADDRESS>
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmds

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"log"
)

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the RootCmd.
func Execute() {
	if err := RootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

// RootCmd represents the base command when called without any subcommands
var RootCmd = &cobra.Command{
	Use:   "esdump",
	Short: "es import export",
	Long: `es import export `,
	// Uncomment the following line if your bare application
	// has an action associated with it:
		Run: func(cmd *cobra.Command, args []string) {
			cmd.Usage()
		},
		PersistentPreRun:func(cmd *cobra.Command, args []string){
			log.SetOutput(os.Stderr)
			setupLogTimezone()
			log.Println("binary version:",Version,"build Time:",BuildTime)
			log.Println("execute ",cmd.Use)
			timeStart=time.Now()
		},
		PersistentPostRunE: func(cmd *cobra.Command, args []string) error {
			log.Printf("%s time spend %s",cmd.Use,time.Now().Sub(timeStart).String())
			return nil
		},
}
var timeStart time.Time

var EsUrl string
var IndexName string

// LogTimezone はログのタイムスタンプに使うタイムゾーンである。
//
// 既定の log パッケージはタイムゾーンを表示しない。ローカルタイムで出力される
// ため、TZ が JST の環境では JST になるが、表記がないため UTC と読み違える。
// ログを別環境で読む場合や、複数のホストのログを並べる場合に取り違える。
var LogTimezone string

// setupLogTimezone はログのタイムスタンプのタイムゾーンを設定する。
//
// PersistentPreRun から呼ぶ。フラグの解析後でなければ LogTimezone が
// 既定値のままになるため、init() では設定できない。
func setupLogTimezone() {
	if LogTimezone == "" {
		return
	}

	loc, err := time.LoadLocation(LogTimezone)
	if err != nil {
		// 解析できないタイムゾーンは異常終了させる。既定の書式に落として
		// 続行すると、意図と違うタイムゾーンで長時間のログが残る。
		log.Fatalf("cannot load timezone %q: %s", LogTimezone, err)
	}
	time.Local = loc
}

func init() {
	log.SetOutput(os.Stderr)
	//log.Println("cobra root init")
	 RootCmd.PersistentFlags().StringVar(&EsUrl,"es","http://localhost:9200","es url")
	RootCmd.PersistentFlags().StringVar(&IndexName,"index","my_index","index name")
	RootCmd.PersistentFlags().StringVar(&LogTimezone,"log-timezone","","timezone for log timestamps (IANA name, e.g. Asia/Tokyo, UTC); empty uses the local timezone")
	RootCmd.MarkFlagRequired("es")
	RootCmd.MarkFlagRequired("index")
	RootCmd.AddCommand(versionCmd)
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "print version",
	Long:  `print version`,
	Run: func(cmd *cobra.Command, args []string) {
	},
}

