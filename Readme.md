- elasticsearch  import export very quick.
- write in go 
- export ~~is~~ gziped 
- speedup to 84 times than nodejs-[elasticsearch-dump](https://github.com/elasticsearch-dump/elasticsearch-dump)
- performance test: with small document the export speed is 14M/s, bigger document the speed up to 27M/s. 
 

> [!IMPORTANT]
> This repository is a **fork** of [wxf4150/esdump](https://github.com/wxf4150/esdump).
> The export output format is **not compatible** with the upstream project, and
> several flags and bug fixes have been added on top of it.
> See **[FORK.md](FORK.md)** for the complete list of differences.

 usage:
 ```shell script
go build 
./esdump export --index my_index  -o ./my_index.json.gz  #export  my_index to file  myindex.json.gz
./esdump import --index my_index1 -i ./my_index.json.gz  #import   file  my_index.json.gz  to my_index1
./esdump export --es http://server1:9200 -o - --index tmp_index | ssh server2 ./esdump import --es http://localhost:9200 --index tmp_index1  -i - #export server1 tmp_index to stdout and pipe to next Import

#export which match body
./esdump  export --es http://server1:9200  --MatchBody '{"range": {"eventTimestamp": {"gte": "2021-05-07T10:32:20.170178Z"}}}' --index events
#export data a minites ago
./esdump export --es http://server1:9200 -m "{\"range\": {\"eventTimestamp\": {\"gte\": \"`date  -d  "1 minutes ago" +%s `000\"}}}" --index events -o - | ./esdump import --index events -i - 
 ./esdump -h
./esdump import -h
./esdump expport -h
 ```

**note**:
- when use import;  you should setting the target index's _mapping .

## Differences from upstream

This fork adds parallel export (`--slices`, `--size`), configurable log
intervals (`--progress-every`, `--pages-every`), an explicit log timezone
(`--log-timezone`), and a number of bug fixes around exit codes and truncated
output. The export format also changed to match elasticsearch-dump, so dumps
produced here cannot be read by the upstream esdump.

Every change, with the reasoning and the measured numbers, is documented in
**[FORK.md](FORK.md)**.


command help:
```shell script
./esdump -h
es import export

Usage:
  esdump [flags]
  esdump [command]

Available Commands:
  export      elasticsearch export
  help        Help about any command
  import      elasticsearch import
  version     print version

Flags:
      --es string             es url (default "http://localhost:9200")
  -h, --help                  help for esdump
      --index string          index name (default "my_index")
      --log-timezone string   timezone for log timestamps (IANA name, e.g. Asia/Tokyo, UTC); empty uses the local timezone

Use "esdump [command] --help" for more information about a command.

./esdump export -h
elasticsearch export

Usage:
  esdump export [flags]

Flags:
  -m, --MatchBody string     MatchBody, empty for match_all; example:{"range": {"timestamp": {"gte": "2021-04-20"}}} (default "{\"match_all\":{}}")
  -c, --c int                set the max amount of documents to be exported; default(0) will exported all matched document; 
      --gzip                 enable gzip; to disable gzip add parameter "--gzip=false" (default true)
  -h, --help                 help for export
  -o, --o string             export dest filename; use - for stdout (default "./tmp_export.json.gz")
      --pages-every int      log fetch time every N pages; 0 disables fetch time logging (default 1000)
      --progress-every int   log export progress every N documents; 0 disables progress logging (default 10000)
      --size int             scroll page size; number of documents fetched per request (default 100)
      --slices int           number of sliced scrolls to read in parallel; 1 disables slicing; match the index shard count (default 1)

Global Flags:
      --es string             es url (default "http://localhost:9200")
      --index string          index name (default "my_index")
      --log-timezone string   timezone for log timestamps (IANA name, e.g. Asia/Tokyo, UTC); empty uses the local timezone

./esdump import -h
elasticsearch import

Usage:
  esdump import [flags]

Flags:
      --gzip       enable gzip; to disable gzip add parameter "--gzip=false" (default true)
  -h, --help       help for import
  -i, --i string   import filename; use - for stdin (default "./tmp_import.json.gz")

Global Flags:
      --es string             es url (default "http://localhost:9200")
      --index string          index name (default "my_index")
      --log-timezone string   timezone for log timestamps (IANA name, e.g. Asia/Tokyo, UTC); empty uses the local timezone
```


why it so quick?

- esdump write in golang .
- when export,  esdump never decode/encode the res.hits.source to an json object, it only save the res.hits.source bytes to gzip stream directly.  
-  nodejs(elasticsearch-dump) may  decode/encode all "elasticsearch respose body" to json object when export.

note:  res.hits.source is the document body from elasticsearch respose body


the export format is below and very simple:
```shell script
{"_index":"my_index","_id":"163820696","_score":1,"_source":{"id":163820696,"asset":"","imageUrl":""}}
{"_index":"my_index","_id":"163820697","_score":1,"_source":{"id":163820697,"asset":"","imageUrl":""}}
{"_index":"my_index","_id":"163820698","_score":1,"_source":{"id":163820698,"asset":"","imageUrl":""}}
...
...
```
one document one row.
the document body is in the `_source` field.

> [!WARNING]
> These field names differ from upstream, which writes `{"ID":...,"RawData":...}`.
> The names here match an Elasticsearch response hit, so the output lines up with
> [elasticsearch-dump](https://github.com/elasticsearch-dump/elasticsearch-dump).
> Dumps written by this fork cannot be imported by the upstream esdump.
> See [FORK.md](FORK.md#出力フォーマットの変更).

sorry my bad english

## Install

Download a prebuilt binary from [Releases](https://github.com/hiboma/esdump/releases).

```shell script
# example: linux amd64
TAG=$(curl -s https://api.github.com/repos/hiboma/esdump/releases/latest | grep -o '"tag_name": *"[^"]*"' | cut -d'"' -f4)
curl -sL "https://github.com/hiboma/esdump/releases/download/${TAG}/esdump_${TAG#v}_linux_amd64.tar.gz" | tar xz
./esdump version
```

Archive names carry the version without the leading `v`, while the tag keeps it:
tag `v1.2.3` produces `esdump_1.2.3_linux_amd64.tar.gz`.

Binaries are built for `darwin/amd64`, `darwin/arm64`, `linux/amd64` and `linux/arm64`.
`checksums.txt` is attached to every release.

## Release flow

Releases are automated with [tagpr](https://github.com/Songmu/tagpr) and
[GoReleaser](https://goreleaser.com/).

1. Merge changes into `master`.
2. tagpr opens (or updates) a release pull request that bumps the version and
   updates `CHANGELOG.md`.
   Add a `minor` or `major` label to that pull request to control the bump;
   without a label the patch version is bumped.
3. Merging the release pull request makes tagpr push the `vX.Y.Z` tag, and the
   same workflow run then invokes GoReleaser to build the binaries and publish
   the GitHub Release.

This requires **Allow GitHub Actions to create and approve pull requests** to be
enabled under Settings > Actions > General. GitHub does not separate creating a
pull request from approving one, so tagpr cannot open its release pull request
without it. Workflow permissions themselves can stay at read-only, since
`tagpr.yml` declares what it needs per job.

Workflows on the release pull request itself sit at `action_required` until
someone approves them — the same loop-prevention rule that stops the pushed tag
from firing a `push` event. The `test` workflow has already run on `master` over
the same code, and the release pull request only changes `CHANGELOG.md` and the
version, so approving it is optional.

To verify the release configuration locally:

```shell script
make release-check      # validate .goreleaser.yml
make release-snapshot   # build artifacts into dist/ without publishing
```

## Tests

Unit tests need nothing but Go. Integration tests talk to a real OpenSearch,
because sliced scroll behaviour — whether every document is returned exactly
once, and where the page boundaries fall — cannot be checked against a mock.

```shell script
make test/short        # unit tests only; no container needed
make test/integration  # start OpenSearch, run every test, keep it running
make test/down         # stop and remove the container
```

`make test` runs everything and skips the integration tests when OpenSearch is
not reachable. Set `ESDUMP_TEST_ES_REQUIRED=1` to turn that skip into a
failure, which is what CI does so that integration tests cannot quietly stop
running.

The container listens on `127.0.0.1:19217`. Port 9200 is deliberately avoided
so it does not clash with another Elasticsearch or OpenSearch on the same
machine. Override the endpoint with `ESDUMP_TEST_ES`.

The integration tests delete and recreate the indices they seed, so two guards
stand in the way of doing that to something you care about: the endpoint has to
resolve to a loopback host unless `ESDUMP_TEST_ALLOW_REMOTE=1` says otherwise,
and every seeded index name has to start with `esdump_test_`. A health check
cannot serve as that guard — a production cluster answers it more reliably than
a container you forgot to start.

### Seeding a larger index by hand

The integration tests seed a few hundred documents each, which is enough for
correctness but not for measuring throughput. To seed a larger index:

```shell script
make test/seed COUNT=1000000 SHARDS=5
./esdump export --es http://127.0.0.1:19217 --index esdump_bench \
  --size 1000 --slices 5 -o /tmp/bench.json.gz
```
