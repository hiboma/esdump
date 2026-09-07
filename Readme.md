- elasticsearch  import export very quick.
- write in go 
- export ~~is~~ gziped 
- speedup to 84 times than nodejs-[elasticsearch-dump](https://github.com/elasticsearch-dump/elasticsearch-dump)
- performance test: with small document the export speed is 14M/s, bigger document the speed up to 27M/s. 
 
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

## logging options

Exporting a large index writes a lot of progress lines. `export` logs one line
every 10,000 documents by default, so an index of 200 million documents produces
more than 20,000 lines. That can fill up log rotation with progress lines alone
and bury errors. Use `--progress-every` to widen the interval, or `0` to turn
progress logging off.

```shell script
# one line every 100,000 documents instead of every 10,000
./esdump export --index my_index -o - --progress-every 100000

# no progress lines and no fetch time lines at all
./esdump export --index my_index -o - --progress-every 0 --pages-every 0
```

Log timestamps have no timezone in them. They are written in the local timezone,
so the same line means different things depending on where it was produced, and
`2026/09/07 12:42:55` cannot be told apart from a UTC timestamp. Use
`--log-timezone` to pin the timezone explicitly.

```shell script
./esdump export --index my_index -o - --log-timezone Asia/Tokyo
./esdump export --index my_index -o - --log-timezone UTC
```

An unknown timezone name exits with an error rather than falling back, so a long
running export cannot end up with timestamps in an unintended timezone.

The defaults keep the current behaviour: progress every 10,000 documents, fetch
time every 1,000 pages, and timestamps in the local timezone.

## parallel export with sliced scroll

A scroll cannot be parallelised on its own. Each request needs the scroll ID
returned by the previous response, so pages are fetched strictly one after
another and the round trip time sets the ceiling on throughput.

Elasticsearch and OpenSearch solve this with [sliced
scroll](https://www.elastic.co/guide/en/elasticsearch/reference/current/paginate-search-results.html#slice-scroll):
the index is split into N disjoint subsets that can be read as N independent
scrolls at the same time. `--slices` starts one goroutine per slice and feeds
them into the same writer.

```shell script
# read the index as 3 parallel slices, 1000 documents per request
./esdump export --index my_index -o - --slices 3 --size 1000
```

**Match `--slices` to the number of primary shards.** Each slice still visits
every document in the shards it covers, so asking for more slices than shards
makes the same data be scanned several times. Measured on a 3 shard index of
192 million documents, exporting 1 million documents (median of 3 runs):

| slices | size | time | speedup |
| --- | --- | --- | --- |
| 1 | 100 | 245.0 s | 1.00 |
| 3 | 100 | 91.2 s | 2.69 |
| 1 | 1000 | 160.8 s | 1.52 |
| **3** | **1000** | **72.9 s** | **3.36** |
| 6 | 1000 | 243.9 s | 1.05 |

Six slices over three shards is barely faster than not slicing at all.

`--size` sets how many documents each request returns. Raising it helps on its
own, but much less than slicing does, and the two overlap: both of them cut the
number of round trips, so their gains do not multiply.

The defaults are `--slices 1` and `--size 100`, which is the behaviour of
earlier versions.


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

Flags:
      --es string      es url (default "http://localhost:9200")
  -h, --help           help for esdump
      --index string   index name (default "my_index")

Use "esdump [command] --help" for more information about a command.


./esdump  export -h
elasticsearch export

Usage:
  esdump export [flags]

Flags:
  -m, --MatchBody string   MatchBody, empty for match_all; example:{"range": {"timestamp": {"gte": "2021-04-20"}}} (default "{\"match_all\":{}}")
  -h, --help       help for export
      --o string   export desk filename; use - for stdout (default "./tmp_export.json.gz")

Global Flags:
      --es string      es url (default "http://localhost:9200")
      --index string   index name (default "my_index")


 ./esdump import -h
elasticsearch import

Usage:
  esdump import [flags]

Flags:
  -h, --help       help for import
      --i string   import  filename; use - for stdin (default "./tmp_import.json.gz")

Global Flags:
      --es string      es url (default "http://localhost:9200")
      --index string   index name (default "my_index")

```


why it so quick?

- esdump write in golang .
- when export,  esdump never decode/encode the res.hits.source to an json object, it only save the res.hits.source bytes to gzip stream directly.  
-  nodejs(elasticsearch-dump) may  decode/encode all "elasticsearch respose body" to json object when export.

note:  res.hits.source is the document body from elasticsearch respose body


the export format is below and very simple:
```shell script
{"ID":"163820696","RawData":{"id":163820696,"asset":"","imageUrl":""}}
{"ID":"163820697","RawData":{"id":163820696,"asset":"","imageUrl":""}}
{"ID":"163820698","RawData":{"id":163820696,"asset":"","imageUrl":""}}
...
...
```
one document one row.
the field "RawData" in the  document.

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

To verify the release configuration locally:

```shell script
make release-check      # validate .goreleaser.yml
make release-snapshot   # build artifacts into dist/ without publishing
```
