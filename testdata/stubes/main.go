package main

// esdump の測定用スタブ Elasticsearch。
//
// Elasticsearch を立てずに 100 万件規模の取得を測れる。sliced scroll の
// 分割が効いているか、変更が性能を落としていないかを手元で確かめられる。
//
// testdata/ に置いてあるため go build ./... の対象にならない。
//
//	go run ./testdata/stubes &
//	./esdump export --es http://127.0.0.1:19207 --index idx --gzip=false //	  --slices 3 --size 1000 -o - > /dev/null
//
// 環境変数
//
//	STUB_TOTAL      スライスあたりの件数 (既定 1000000)
//	STUB_PORT       待ち受けポート (既定 19207)
//	STUB_DELAY_MS   1 リクエストあたりの遅延。遅延なしでは往復の待ちが
//	                支配的にならず --size の効果が再現しない
//	STUB_DEBUG      受けたリクエストをログに出す
//
// 実装で外しやすい点が 2 つある。
//
//   - size はボディではなくクエリパラメータ (?scroll=5m&size=1000) で来る
//   - 継続要求 (/_search/scroll) は size を含まない。スライスごとに覚えて
//     おかないと 2 ページ目から既定の 100 に落ちる
//
// どちらも測定条件を静かに取り違える。件数が期待と合うかを必ず見ること。

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	total  = envInt("STUB_TOTAL", 1000000) // スライスあたりの件数
	mu     sync.Mutex
	served = map[string]int{}
	// scroll の継続要求 (/_search/scroll) は size を含まない。最初の検索で
	// 決まった値を引き継ぐのが Elasticsearch の仕様である。スライスごとに
	// 覚えておかないと、2 ページ目以降で既定の 100 に落ちる。
	sizes   = map[string]int{}
	docBody string
)

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func main() {
	// wazuh-alerts に近い大きさの文書を用意する (約 700 バイト)
	docBody = strings.Repeat("x", 400)

	mux := http.NewServeMux()
	mux.HandleFunc("/", handle)

	addr := "127.0.0.1:" + strconv.Itoa(envInt("STUB_PORT", 19207))
	log.Println("stub es listening on", addr, "total per slice:", total)
	srv := &http.Server{Addr: addr, Handler: mux}
	log.Fatal(srv.ListenAndServe())
}

func handle(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		writeJSON(w, map[string]interface{}{
			"name": "stub", "cluster_name": "stub",
			"cluster_uuid": "stubstubstubstubstubst",
			"version": map[string]interface{}{
				"number": "7.10.2", "build_flavor": "default", "build_type": "tar",
				"build_hash": "0", "build_date": "2021-01-01T00:00:00.000Z",
				"build_snapshot": false, "lucene_version": "8.7.0",
				"minimum_wire_compatibility_version":  "6.8.0",
				"minimum_index_compatibility_version": "6.0.0-beta1",
			},
			"tagline": "You Know, for Search",
		})
	case http.MethodPost:
		var q map[string]interface{}
		json.NewDecoder(r.Body).Decode(&q)
		if os.Getenv("STUB_DEBUG") != "" {
			b, _ := json.Marshal(q)
			log.Printf("POST %s?%s body=%s", r.URL.Path, r.URL.RawQuery, b)
		}

		key := "0"
		if sl, ok := q["slice"].(map[string]interface{}); ok {
			key = fmt.Sprintf("%v", sl["id"])
		}
		if sid, ok := q["scroll_id"].(string); ok {
			key = strings.TrimPrefix(sid, "s")
		}

		// size はクエリパラメータで来る (?scroll=5m&size=1000)。
		// ボディから読もうとすると既定の 100 のままになり、測定条件を
		// 取り違える。
		reqSize := 0
		if v := r.URL.Query().Get("size"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				reqSize = n
			}
		}

		mu.Lock()
		if reqSize > 0 {
			sizes[key] = reqSize
		}
		size := sizes[key]
		if size <= 0 {
			size = 100
		}
		done := served[key]
		take := size
		if rem := total - done; rem < take {
			take = rem
		}
		if take < 0 {
			take = 0
		}
		served[key] = done + take
		mu.Unlock()

		// 1 リクエストあたりの遅延を入れる。
		//
		// 遅延なしでは往復の待ち時間が支配的にならず、--size の効果が
		// 再現しない。本番は OpenSearch の検索処理が入るため往復ごとに
		// 待ちが生じる。
		if d := os.Getenv("STUB_DELAY_MS"); d != "" {
			if ms, err := strconv.Atoi(d); err == nil && ms > 0 {
				time.Sleep(time.Duration(ms) * time.Millisecond)
			}
		}

		hits := make([]map[string]interface{}, 0, take)
		for i := 0; i < take; i++ {
			hits = append(hits, map[string]interface{}{
				"_index": "idx", "_id": fmt.Sprintf("%s-%d", key, done+i), "_score": 1.0,
				"_source": map[string]interface{}{
					"timestamp": "2026-09-07T12:34:56.789+0000",
					"full_log":  docBody,
					"agent":     map[string]string{"id": "012345", "name": "web-host-01.example.jp"},
					"rule":      map[string]interface{}{"level": 5, "id": "112000"},
				},
			})
		}
		writeJSON(w, map[string]interface{}{
			"_scroll_id": "s" + key, "took": 1, "timed_out": false,
			"_shards": map[string]int{"total": 3, "successful": 3, "skipped": 0, "failed": 0},
			"hits": map[string]interface{}{
				"total": map[string]interface{}{"value": total, "relation": "eq"},
				"hits":  hits,
			},
		})
	case http.MethodDelete:
		writeJSON(w, map[string]interface{}{"succeeded": true, "num_freed": 1})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
