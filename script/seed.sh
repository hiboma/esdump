#!/usr/bin/env bash
#
# 手動検証用のインデックスを seed する。
#
# 統合テストは自分で seed するため、このスクリプトは不要である。
# sliced scroll の速度を実測したい場合や、export の出力を目で確認したい
# 場合に使う。テストの seed は数百件で、性能の傾向を見るには足りない。
#
#   ./script/seed.sh                    # 既定 (100000 件, 3 シャード)
#   ./script/seed.sh 1000000 5          # 100 万件, 5 シャード
#   ES=http://other:9200 ./script/seed.sh
set -euo pipefail

ES="${ES:-http://127.0.0.1:19217}"
INDEX="${INDEX:-esdump_bench}"
COUNT="${1:-100000}"
SHARDS="${2:-3}"
# bulk 1 リクエストあたりの件数である。大きくしすぎると OpenSearch 側の
# http.max_content_length (既定 100mb) に当たる。
BATCH="${BATCH:-5000}"

# 数値であることを確認する。COUNT と SHARDS は JSON の値位置と算術式に
# 入るため、非数値だと意図しない設定キーの注入や、投入ループの空回りに
# なる。手元専用のスクリプトだが、書き間違いを早い段階で明確に落とす。
for var in COUNT SHARDS BATCH; do
  val="${!var}"
  case "${val}" in
    '' | *[!0-9]*)
      echo "${var} は正の整数である必要がある: ${val}" >&2
      exit 1
      ;;
  esac
  if [ "${val}" -lt 1 ] && [ "${var}" != "COUNT" ]; then
    echo "${var} は 1 以上である必要がある: ${val}" >&2
    exit 1
  fi
done

echo "es=${ES} index=${INDEX} count=${COUNT} shards=${SHARDS}"

curl -sf -X DELETE "${ES}/${INDEX}" >/dev/null 2>&1 || true
curl -sf -X PUT "${ES}/${INDEX}" \
  -H 'Content-Type: application/json' \
  -d "{\"settings\":{\"number_of_shards\":${SHARDS},\"number_of_replicas\":0,\"refresh_interval\":\"-1\"}}" >/dev/null
echo "created index ${INDEX}"

# refresh_interval を切って投入する。既定の 1 秒間隔の refresh は大量投入の
# スループットを大きく落とす。投入後に戻して 1 度だけ refresh する。

seeded=0
while [ "${seeded}" -lt "${COUNT}" ]; do
  remain=$((COUNT - seeded))
  n=$((remain < BATCH ? remain : BATCH))
  # NDJSON を組み立てて 1 リクエストで送る。1 件ずつの POST では
  # 100 万件の投入が現実的な時間で終わらない。
  awk -v start="${seeded}" -v n="${n}" -v idx="${INDEX}" 'BEGIN{
    for (i = 0; i < n; i++) {
      id = start + i
      printf "{\"index\":{\"_index\":\"%s\",\"_id\":\"%d\"}}\n", idx, id
      printf "{\"n\":%d,\"name\":\"doc-%d\",\"pad\":\"%s\"}\n", id, id, "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
    }
  }' | curl -sf -X POST "${ES}/_bulk" \
        -H 'Content-Type: application/x-ndjson' \
        --data-binary @- \
        -o /dev/null
  seeded=$((seeded + n))
  printf "\rseeded %d/%d" "${seeded}" "${COUNT}"
done
echo ""

curl -sf -X PUT "${ES}/${INDEX}/_settings" \
  -H 'Content-Type: application/json' \
  -d '{"index":{"refresh_interval":"1s"}}' >/dev/null
curl -sf -X POST "${ES}/${INDEX}/_refresh" >/dev/null

actual=$(curl -sf "${ES}/${INDEX}/_count" | sed -E 's/.*"count":([0-9]+).*/\1/')
echo "done: ${INDEX} has ${actual} docs"
if [ "${actual}" != "${COUNT}" ]; then
  echo "警告: 件数が一致しない (want ${COUNT}, got ${actual})" >&2
  exit 1
fi
