// Copyright 2018 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package changefeedccl

import (
	"bytes"
	"context"
	gosql "database/sql"
	"fmt"
	"hash"
	"hash/fnv"
	"io"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Shopify/sarama"
	"github.com/cockroachdb/cockroach/pkg/ccl/storageccl"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/builtins"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/util/bufalloc"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/cockroach/pkg/util/uuid"
	"github.com/pkg/errors"
)

// Sink is an abstraction for anything that a changefeed may emit into.
type Sink interface {
	// EmitRow enqueues a row message for asynchronous delivery on the sink. An
	// error may be returned if a previously enqueued message has failed.
	EmitRow(
		ctx context.Context,
		table *sqlbase.TableDescriptor,
		key, value []byte,
		updated hlc.Timestamp,
	) error
	// EmitResolvedTimestamp enqueues a resolved timestamp message for
	// asynchronous delivery on every partition of every topic that has been
	// seen by EmitRow. The list of partitions used may be stale. An error may
	// be returned if a previously enqueued message has failed.
	EmitResolvedTimestamp(ctx context.Context, encoder Encoder, resolved hlc.Timestamp) error
	// Flush blocks until every message enqueued by EmitRow and
	// EmitResolvedTimestamp with a timestamp >= ts has been acknowledged by the
	// sink. This is also a guarantee that rows that come in after will have an
	// updated timestamp <= ts (which can be useful for gc inside the sink). If
	// an error is returned, no guarantees are given about which messages have
	// been delivered or not delivered.
	Flush(ctx context.Context, ts hlc.Timestamp) error
	// Close does not guarantee delivery of outstanding messages.
	Close() error
}

func getSink(
	sinkURI string,
	opts map[string]string,
	targets jobspb.ChangefeedTargets,
	settings *cluster.Settings,
) (Sink, error) {
	u, err := url.Parse(sinkURI)
	if err != nil {
		return nil, err
	}
	q := u.Query()

	// Use a function here to delay creation of the sink until after we've done
	// all the parameter verification.
	var makeSink func() (Sink, error)
	switch u.Scheme {
	case sinkSchemeBuffer:
		makeSink = func() (Sink, error) { return &bufferSink{}, nil }
	case sinkSchemeKafka:
		kafkaTopicPrefix := q.Get(sinkParamTopicPrefix)
		q.Del(sinkParamTopicPrefix)
		schemaTopic := q.Get(sinkParamSchemaTopic)
		q.Del(sinkParamSchemaTopic)
		if schemaTopic != `` {
			return nil, errors.Errorf(`%s is not yet supported`, sinkParamSchemaTopic)
		}
		makeSink = func() (Sink, error) {
			return makeKafkaSink(kafkaTopicPrefix, u.Host, targets)
		}
	case `experimental-s3`, `experimental-gs`, `experimental-nodelocal`, `experimental-http`,
		`experimental-https`, `experimental-azure`:
		sinkURI = strings.TrimPrefix(sinkURI, `experimental-`)
		bucketSizeStr := q.Get(sinkParamBucketSize)
		q.Del(sinkParamBucketSize)
		if bucketSizeStr == `` {
			return nil, errors.Errorf(`sink param %s is required`, sinkParamBucketSize)
		}
		bucketSize, err := time.ParseDuration(bucketSizeStr)
		if err != nil {
			return nil, err
		}
		makeSink = func() (Sink, error) {
			return makeCloudStorageSink(sinkURI, bucketSize, settings, opts)
		}
	case sinkSchemeExperimentalSQL:
		// Swap the changefeed prefix for the sql connection one that sqlSink
		// expects.
		u.Scheme = `postgres`
		// TODO(dan): Make tableName configurable or based on the job ID or
		// something.
		tableName := `sqlsink`
		makeSink = func() (Sink, error) {
			return makeSQLSink(u.String(), tableName, targets)
		}
		// Remove parameters we know about for the unknown parameter check.
		q.Del(`sslcert`)
		q.Del(`sslkey`)
		q.Del(`sslmode`)
		q.Del(`sslrootcert`)
	default:
		return nil, errors.Errorf(`unsupported sink: %s`, u.Scheme)
	}

	for k := range q {
		return nil, errors.Errorf(`unknown sink query parameter: %s`, k)
	}

	s, err := makeSink()
	if err != nil {
		return nil, err
	}
	return s, nil
}

// kafkaSink emits to Kafka asynchronously. It is not concurrency-safe; all
// calls to Emit and Flush should be from the same goroutine.
type kafkaSink struct {
	// TODO(dan): This uses the shopify kafka producer library because the
	// official confluent one depends on librdkafka and it didn't seem worth it
	// to add a new c dep for the prototype. Revisit before 2.1 and check
	// stability, performance, etc.
	kafkaTopicPrefix string
	client           sarama.Client
	producer         sarama.AsyncProducer
	topics           map[string]struct{}

	lastMetadataRefresh time.Time

	stopWorkerCh chan struct{}
	worker       sync.WaitGroup
	scratch      bufalloc.ByteAllocator

	// Only synchronized between the client goroutine and the worker goroutine.
	mu struct {
		syncutil.Mutex
		inflight int64
		flushErr error
		flushCh  chan struct{}
	}
}

func makeKafkaSink(
	kafkaTopicPrefix string, bootstrapServers string, targets jobspb.ChangefeedTargets,
) (Sink, error) {
	sink := &kafkaSink{
		kafkaTopicPrefix: kafkaTopicPrefix,
	}
	sink.topics = make(map[string]struct{})
	for _, t := range targets {
		sink.topics[kafkaTopicPrefix+SQLNameToKafkaName(t.StatementTimeName)] = struct{}{}
	}

	config := sarama.NewConfig()
	config.Producer.Return.Successes = true
	config.Producer.Partitioner = newChangefeedPartitioner

	// When we emit messages to sarama, they're placed in a queue (as does any
	// reasonable kafka producer client). When our sink's Flush is called, we
	// have to wait for all buffered and inflight requests to be sent and then
	// acknowledged. Quite unfortunately, we have no way to hint to the producer
	// that it should immediately send out whatever is buffered. This
	// configuration can have a dramatic impact on how quickly this happens
	// naturally (and some configurations will block forever!).
	//
	// We can configure the producer to send out its batches based on number of
	// messages and/or total buffered message size and/or time. If none of them
	// are set, it uses some defaults, but if any of the three are set, it does
	// no defaulting. Which means that if `Flush.Messages` is set to 10 and
	// nothing else is set, then 9/10 times `Flush` will block forever. We can
	// work around this by also setting `Flush.Frequency` but a cleaner way is
	// to set `Flush.Messages` to 1. In the steady state, this sends a request
	// with some messages, buffers any messages that come in while it is in
	// flight, then sends those out.
	config.Producer.Flush.Messages = 1

	// This works around what seems to be a bug in sarama where it isn't
	// computing the right value to compare against `Producer.MaxMessageBytes`
	// and the server sends it back with a "Message was too large, server
	// rejected it to avoid allocation" error. The other flush tunings are
	// hints, but this one is a hard limit, so it's useful here as a workaround.
	//
	// This workaround should probably be something like setting
	// `Producer.MaxMessageBytes` to 90% of it's value for some headroom, but
	// this workaround is the one that's been running in roachtests and I'd want
	// to test this one more before changing it.
	config.Producer.Flush.MaxMessages = 1000

	var err error
	sink.client, err = sarama.NewClient(strings.Split(bootstrapServers, `,`), config)
	if err != nil {
		err = errors.Wrapf(err, `connecting to kafka: %s`, bootstrapServers)
		return nil, &retryableSinkError{cause: err}
	}
	sink.producer, err = sarama.NewAsyncProducerFromClient(sink.client)
	if err != nil {
		err = errors.Wrapf(err, `connecting to kafka: %s`, bootstrapServers)
		return nil, &retryableSinkError{cause: err}
	}

	sink.start()
	return sink, nil
}

func (s *kafkaSink) start() {
	s.stopWorkerCh = make(chan struct{})
	s.worker.Add(1)
	go s.workerLoop()
}

// Close implements the Sink interface.
func (s *kafkaSink) Close() error {
	close(s.stopWorkerCh)
	s.worker.Wait()

	// If we're shutting down, we don't care what happens to the outstanding
	// messages, so ignore this error.
	_ = s.producer.Close()
	// s.client is only nil in tests.
	if s.client != nil {
		return s.client.Close()
	}
	return nil
}

// EmitRow implements the Sink interface.
func (s *kafkaSink) EmitRow(
	ctx context.Context, table *sqlbase.TableDescriptor, key, value []byte, _ hlc.Timestamp,
) error {
	topic := s.kafkaTopicPrefix + SQLNameToKafkaName(table.Name)
	if _, ok := s.topics[topic]; !ok {
		return errors.Errorf(`cannot emit to undeclared topic: %s`, topic)
	}

	msg := &sarama.ProducerMessage{
		Topic: topic,
		Key:   sarama.ByteEncoder(key),
		Value: sarama.ByteEncoder(value),
	}
	return s.emitMessage(ctx, msg)
}

// EmitResolvedTimestamp implements the Sink interface.
func (s *kafkaSink) EmitResolvedTimestamp(
	ctx context.Context, encoder Encoder, resolved hlc.Timestamp,
) error {
	// Periodically ping sarama to refresh its metadata. This means talking to
	// zookeeper, so it shouldn't be done too often, but beyond that this
	// constant was picked pretty arbitrarily.
	//
	// TODO(dan): Add a test for this. We can't right now (2018-11-13) because
	// we'd need to bump sarama, but that's a bad idea while we're still
	// actively working on stability. At the same time, revisit this tuning.
	const metadataRefreshMinDuration = time.Minute
	if timeutil.Since(s.lastMetadataRefresh) > metadataRefreshMinDuration {
		topics := make([]string, 0, len(s.topics))
		for topic := range s.topics {
			topics = append(topics, topic)
		}
		if err := s.client.RefreshMetadata(topics...); err != nil {
			return err
		}
		s.lastMetadataRefresh = timeutil.Now()
	}

	for topic := range s.topics {
		payload, err := encoder.EncodeResolvedTimestamp(topic, resolved)
		if err != nil {
			return err
		}
		s.scratch, payload = s.scratch.Copy(payload, 0 /* extraCap */)

		// sarama caches this, which is why we have to periodically refresh the
		// metadata above. Staleness here does not impact correctness. Some new
		// partitions will miss this resolved timestamp, but they'll eventually
		// be picked up and get later ones.
		partitions, err := s.client.Partitions(topic)
		if err != nil {
			return err
		}
		for _, partition := range partitions {
			msg := &sarama.ProducerMessage{
				Topic:     topic,
				Partition: partition,
				Key:       nil,
				Value:     sarama.ByteEncoder(payload),
			}
			if err := s.emitMessage(ctx, msg); err != nil {
				return err
			}
		}
	}
	return nil
}

// Flush implements the Sink interface.
func (s *kafkaSink) Flush(ctx context.Context, _ hlc.Timestamp) error {
	// Ignore the timestamp and flush everything, which necessarily means that
	// we've flushed everything >= the timestamp.

	flushCh := make(chan struct{}, 1)

	s.mu.Lock()
	inflight := s.mu.inflight
	flushErr := s.mu.flushErr
	s.mu.flushErr = nil
	immediateFlush := inflight == 0 || flushErr != nil
	if !immediateFlush {
		s.mu.flushCh = flushCh
	}
	s.mu.Unlock()

	if immediateFlush {
		if _, ok := flushErr.(*sarama.ProducerError); ok {
			flushErr = &retryableSinkError{cause: flushErr}
		}
		return flushErr
	}

	if log.V(1) {
		log.Infof(ctx, "flush waiting for %d inflight messages", inflight)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-flushCh:
		s.mu.Lock()
		flushErr := s.mu.flushErr
		s.mu.flushErr = nil
		s.mu.Unlock()
		if _, ok := flushErr.(*sarama.ProducerError); ok {
			flushErr = &retryableSinkError{cause: flushErr}
		}
		return flushErr
	}
}

func (s *kafkaSink) emitMessage(ctx context.Context, msg *sarama.ProducerMessage) error {
	s.mu.Lock()
	s.mu.inflight++
	inflight := s.mu.inflight
	s.mu.Unlock()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case s.producer.Input() <- msg:
	}

	if log.V(2) {
		log.Infof(ctx, "emitted %d inflight records to kafka", inflight)
	}
	return nil
}

func (s *kafkaSink) workerLoop() {
	defer s.worker.Done()

	for {
		select {
		case <-s.stopWorkerCh:
			return
		case <-s.producer.Successes():
		case err := <-s.producer.Errors():
			s.mu.Lock()
			if s.mu.flushErr == nil {
				s.mu.flushErr = err
			}
			s.mu.Unlock()
		}

		s.mu.Lock()
		s.mu.inflight--
		if s.mu.inflight == 0 && s.mu.flushCh != nil {
			s.mu.flushCh <- struct{}{}
			s.mu.flushCh = nil
		}
		s.mu.Unlock()
	}
}

type changefeedPartitioner struct {
	hash sarama.Partitioner
}

var _ sarama.Partitioner = &changefeedPartitioner{}
var _ sarama.PartitionerConstructor = newChangefeedPartitioner

func newChangefeedPartitioner(topic string) sarama.Partitioner {
	return &changefeedPartitioner{
		hash: sarama.NewHashPartitioner(topic),
	}
}

func (p *changefeedPartitioner) RequiresConsistency() bool { return true }
func (p *changefeedPartitioner) Partition(
	message *sarama.ProducerMessage, numPartitions int32,
) (int32, error) {
	if message.Key == nil {
		return message.Partition, nil
	}
	return p.hash.Partition(message, numPartitions)
}

const (
	sqlSinkCreateTableStmt = `CREATE TABLE IF NOT EXISTS "%s" (
		topic STRING,
		partition INT,
		message_id INT,
		key BYTES, value BYTES,
		resolved BYTES,
		PRIMARY KEY (topic, partition, message_id)
	)`
	sqlSinkEmitStmt = `INSERT INTO "%s" (topic, partition, message_id, key, value, resolved)`
	sqlSinkEmitCols = 6
	// Some amount of batching to mirror a bit how kafkaSink works.
	sqlSinkRowBatchSize = 3
	// While sqlSink is only used for testing, hardcode the number of
	// partitions to something small but greater than 1.
	sqlSinkNumPartitions = 3
)

// sqlSink mirrors the semantics offered by kafkaSink as closely as possible,
// but writes to a SQL table (presumably in CockroachDB). Currently only for
// testing.
//
// Each emitted row or resolved timestamp is stored as a row in the table. Each
// table gets 3 partitions. Similar to kafkaSink, the order between two emits is
// only preserved if they are emitted to by the same node and to the same
// partition.
type sqlSink struct {
	db *gosql.DB

	tableName string
	topics    map[string]struct{}
	hasher    hash.Hash32

	rowBuf  []interface{}
	scratch bufalloc.ByteAllocator
}

func makeSQLSink(uri, tableName string, targets jobspb.ChangefeedTargets) (*sqlSink, error) {
	if u, err := url.Parse(uri); err != nil {
		return nil, err
	} else if u.Path == `` {
		return nil, errors.Errorf(`must specify database`)
	}
	db, err := gosql.Open(`postgres`, uri)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(fmt.Sprintf(sqlSinkCreateTableStmt, tableName)); err != nil {
		db.Close()
		return nil, err
	}

	s := &sqlSink{
		db:        db,
		tableName: tableName,
		topics:    make(map[string]struct{}),
		hasher:    fnv.New32a(),
	}
	for _, t := range targets {
		s.topics[t.StatementTimeName] = struct{}{}
	}
	return s, nil
}

// EmitRow implements the Sink interface.
func (s *sqlSink) EmitRow(
	ctx context.Context, table *sqlbase.TableDescriptor, key, value []byte, _ hlc.Timestamp,
) error {
	topic := table.Name
	if _, ok := s.topics[topic]; !ok {
		return errors.Errorf(`cannot emit to undeclared topic: %s`, topic)
	}

	// Hashing logic copied from sarama.HashPartitioner.
	s.hasher.Reset()
	if _, err := s.hasher.Write(key); err != nil {
		return err
	}
	partition := int32(s.hasher.Sum32()) % sqlSinkNumPartitions
	if partition < 0 {
		partition = -partition
	}

	var noResolved []byte
	return s.emit(ctx, topic, partition, key, value, noResolved)
}

// EmitResolvedTimestamp implements the Sink interface.
func (s *sqlSink) EmitResolvedTimestamp(
	ctx context.Context, encoder Encoder, resolved hlc.Timestamp,
) error {
	var noKey, noValue []byte
	for topic := range s.topics {
		payload, err := encoder.EncodeResolvedTimestamp(topic, resolved)
		if err != nil {
			return err
		}
		s.scratch, payload = s.scratch.Copy(payload, 0 /* extraCap */)
		for partition := int32(0); partition < sqlSinkNumPartitions; partition++ {
			if err := s.emit(ctx, topic, partition, noKey, noValue, payload); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *sqlSink) emit(
	ctx context.Context, topic string, partition int32, key, value, resolved []byte,
) error {
	// Generate the message id on the client to match the guaranttees of kafka
	// (two messages are only guaranteed to keep their order if emitted from the
	// same producer to the same partition).
	messageID := builtins.GenerateUniqueInt(roachpb.NodeID(partition))
	s.rowBuf = append(s.rowBuf, topic, partition, messageID, key, value, resolved)
	if len(s.rowBuf)/sqlSinkEmitCols >= sqlSinkRowBatchSize {
		var gcTs hlc.Timestamp
		return s.Flush(ctx, gcTs)
	}
	return nil
}

// Flush implements the Sink interface.
func (s *sqlSink) Flush(ctx context.Context, _ hlc.Timestamp) error {
	// Ignore the timestamp and flush everything, which necessarily means that
	// we've flushed everything >= the timestamp.

	if len(s.rowBuf) == 0 {
		return nil
	}

	var stmt strings.Builder
	fmt.Fprintf(&stmt, sqlSinkEmitStmt, s.tableName)
	for i := 0; i < len(s.rowBuf); i++ {
		if i == 0 {
			stmt.WriteString(` VALUES (`)
		} else if i%sqlSinkEmitCols == 0 {
			stmt.WriteString(`),(`)
		} else {
			stmt.WriteString(`,`)
		}
		fmt.Fprintf(&stmt, `$%d`, i+1)
	}
	stmt.WriteString(`)`)
	_, err := s.db.Exec(stmt.String(), s.rowBuf...)
	if err != nil {
		return err
	}
	s.rowBuf = s.rowBuf[:0]
	return nil
}

// Close implements the Sink interface.
func (s *sqlSink) Close() error {
	return s.db.Close()
}

// encDatumRowBuffer is a FIFO of `EncDatumRow`s.
//
// TODO(dan): There's some potential allocation savings here by reusing the same
// backing array.
type encDatumRowBuffer []sqlbase.EncDatumRow

func (b *encDatumRowBuffer) IsEmpty() bool {
	return b == nil || len(*b) == 0
}
func (b *encDatumRowBuffer) Push(r sqlbase.EncDatumRow) {
	*b = append(*b, r)
}
func (b *encDatumRowBuffer) Pop() sqlbase.EncDatumRow {
	ret := (*b)[0]
	*b = (*b)[1:]
	return ret
}

type bufferSink struct {
	buf     encDatumRowBuffer
	alloc   sqlbase.DatumAlloc
	scratch bufalloc.ByteAllocator
	closed  bool
}

// EmitRow implements the Sink interface.
func (s *bufferSink) EmitRow(
	_ context.Context, table *sqlbase.TableDescriptor, key, value []byte, _ hlc.Timestamp,
) error {
	if s.closed {
		return errors.New(`cannot EmitRow on a closed sink`)
	}
	topic := table.Name
	s.buf.Push(sqlbase.EncDatumRow{
		{Datum: tree.DNull}, // resolved span
		{Datum: s.alloc.NewDString(tree.DString(topic))}, // topic
		{Datum: s.alloc.NewDBytes(tree.DBytes(key))},     // key
		{Datum: s.alloc.NewDBytes(tree.DBytes(value))},   //value
	})
	return nil
}

// EmitResolvedTimestamp implements the Sink interface.
func (s *bufferSink) EmitResolvedTimestamp(
	_ context.Context, encoder Encoder, resolved hlc.Timestamp,
) error {
	if s.closed {
		return errors.New(`cannot EmitResolvedTimestamp on a closed sink`)
	}
	var noTopic string
	payload, err := encoder.EncodeResolvedTimestamp(noTopic, resolved)
	if err != nil {
		return err
	}
	s.scratch, payload = s.scratch.Copy(payload, 0 /* extraCap */)
	s.buf.Push(sqlbase.EncDatumRow{
		{Datum: tree.DNull}, // resolved span
		{Datum: tree.DNull}, // topic
		{Datum: tree.DNull}, // key
		{Datum: s.alloc.NewDBytes(tree.DBytes(payload))}, // value
	})
	return nil
}

// Flush implements the Sink interface.
func (s *bufferSink) Flush(_ context.Context, _ hlc.Timestamp) error {
	return nil
}

// Close implements the Sink interface.
func (s *bufferSink) Close() error {
	s.closed = true
	return nil
}

// cloudStorageFormatBucket formats times as YYYYMMDDHHMMSSNNNNNNNNN.
func cloudStorageFormatBucket(t time.Time) string {
	// TODO(dan): Instead do the minimal thing necessary to differentiate times
	// truncated to some bucket size.
	const f = `20060102150405`
	return fmt.Sprintf(`%s%09d`, t.Format(f), t.Nanosecond())
}

type cloudStorageSinkKey struct {
	Bucket   time.Time
	Topic    string
	SchemaID sqlbase.DescriptorVersion
	SinkID   string
	Ext      string
}

func (k cloudStorageSinkKey) Filename() string {
	return fmt.Sprintf(`%s-%s-%d-%s%s`,
		cloudStorageFormatBucket(k.Bucket), k.Topic, k.SchemaID, k.SinkID, k.Ext)
}

// cloudStorageSink emits to files on cloud storage.
//
// The data files are named `<timestamp>_<topic>_<schema_id>_<uniquer>.<ext>`.
//
// `<timestamp>` is truncated to some bucket size, specified by the required
// sink param `bucket_size`. Bucket size is a tradeoff between number of files
// and the end-to-end latency of data being resolved.
//
// `<topic>` corresponds to one SQL table.
//
// `<schema_id>` changes whenever the SQL table schema changes, which allows us
// to guarantee to users that _all entries in a given file have the same
// schema_.
//
// `<uniquer>` is used to keep nodes in a cluster from overwriting each other's
// data and should be ignored by external users. It also keeps a single node
// from overwriting its own data if there are multiple changefeeds, or if a
// changefeed gets canceled/restarted.
//
// `<ext>` implies the format of the file: currently the only option is
// `ndjson`, which means a text file conforming to the "Newline Delimited JSON"
// spec.
//
// Each record in the data files is a value, keys are not included, so the
// `envelope` option must be set to `row`, which is the default. Within a file,
// records are not guaranteed to be sorted by timestamp. A duplicate of some
// record might exist in a different file or even in the same file.
//
// The resolved timestamp files are named `<timestamp>.RESOLVED`. This is
// carefully done so that we can offer the following external guarantee: At any
// given time, if the the files are iterated in lexicographic filename order,
// then encountering any filename containing `RESOLVED` means that everything
// before it is finalized (and thus can be ingested into some other system and
// deleted, included in hive queries, etc). A typical user of cloudStorageSink
// would periodically do exactly this.
//
// Still TODO is writing out data schemas, Avro support, bounding memory usage.
// Eliminating duplicates would be great, but may not be immediately practical.
type cloudStorageSink struct {
	base       *url.URL
	bucketSize time.Duration
	settings   *cluster.Settings
	sinkID     string

	ext           string
	recordDelimFn func(io.Writer) error

	files           map[cloudStorageSinkKey]*bytes.Buffer
	localResolvedTs hlc.Timestamp
}

func makeCloudStorageSink(
	baseURI string, bucketSize time.Duration, settings *cluster.Settings, opts map[string]string,
) (Sink, error) {
	base, err := url.Parse(baseURI)
	if err != nil {
		return nil, err
	}
	// TODO(dan): Each sink needs a unique id for the reasons described in the
	// above docs, but this is a pretty ugly way to do it.
	sinkID := uuid.MakeV4().String()
	s := &cloudStorageSink{
		base:       base,
		bucketSize: bucketSize,
		settings:   settings,
		sinkID:     sinkID,
		files:      make(map[cloudStorageSinkKey]*bytes.Buffer),
	}

	switch formatType(opts[optFormat]) {
	case optFormatJSON:
		// TODO(dan): It seems like these should be on the encoder, but that
		// seems to require a bit of refactoring.
		s.ext = `.ndjson`
		s.recordDelimFn = func(w io.Writer) error {
			_, err := w.Write([]byte{'\n'})
			return err
		}
	default:
		return nil, errors.Errorf(`this sink is incompatible with %s=%s`,
			optFormat, opts[optFormat])
	}

	switch envelopeType(opts[optEnvelope]) {
	case optEnvelopeValueOnly:
	default:
		return nil, errors.Errorf(`this sink is incompatible with %s=%s`,
			optEnvelope, opts[optEnvelope])
	}

	{
		// Sanity check that we can connect.
		ctx := context.Background()
		es, err := storageccl.ExportStorageFromURI(ctx, s.base.String(), settings)
		if err != nil {
			return nil, err
		}
		if err := es.Close(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// EmitRow implements the Sink interface.
func (s *cloudStorageSink) EmitRow(
	_ context.Context, table *sqlbase.TableDescriptor, _, value []byte, updated hlc.Timestamp,
) error {
	if s.files == nil {
		return errors.New(`cannot EmitRow on a closed sink`)
	}

	// localResolvedTs is a guarantee that any rows <= to it are duplicates and
	// we can drop them.
	//
	// TODO(dan): We could actually move this higher up the changefeed stack and
	// do it for all sinks.
	if !s.localResolvedTs.Less(updated) {
		return nil
	}

	// Intentionally throw away the logical part of the timestamp for bucketing.
	key := cloudStorageSinkKey{
		Bucket:   updated.GoTime().Truncate(s.bucketSize),
		Topic:    table.Name,
		SchemaID: table.Version,
		SinkID:   s.sinkID,
		Ext:      s.ext,
	}
	file := s.files[key]
	if file == nil {
		// We could pool the bytes.Buffers if necessary, but we'd need to be
		// careful to bound the size of the memory held by the pool.
		file = &bytes.Buffer{}
		s.files[key] = file
	}

	// TODO(dan): Memory monitoring for this
	if _, err := file.Write(value); err != nil {
		return err
	}
	return s.recordDelimFn(file)
}

// EmitResolvedTimestamp implements the Sink interface.
func (s *cloudStorageSink) EmitResolvedTimestamp(
	ctx context.Context, encoder Encoder, resolved hlc.Timestamp,
) error {
	if s.files == nil {
		return errors.New(`cannot EmitRow on a closed sink`)
	}

	var noTopic string
	payload, err := encoder.EncodeResolvedTimestamp(noTopic, resolved)
	if err != nil {
		return err
	}
	// Don't need to copy payload because we never buffer it anywhere.

	es, err := storageccl.ExportStorageFromURI(ctx, s.base.String(), s.settings)
	if err != nil {
		return err
	}
	defer func() {
		if err := es.Close(); err != nil {
			log.Warningf(ctx, `failed to close %s, resources may have leaked: %s`, s.base.String(), err)
		}
	}()

	// resolving some given time means that every in the _previous_ bucket is
	// finished.
	resolvedBucket := resolved.GoTime().Truncate(s.bucketSize).Add(-time.Nanosecond)
	name := cloudStorageFormatBucket(resolvedBucket) + `.RESOLVED`
	if log.V(1) {
		log.Info(ctx, "writing ", name)
	}

	return es.WriteFile(ctx, name, bytes.NewReader(payload))
}

// Flush implements the Sink interface.
func (s *cloudStorageSink) Flush(ctx context.Context, ts hlc.Timestamp) error {
	if s.files == nil {
		return errors.New(`cannot Flush on a closed sink`)
	}
	if s.localResolvedTs.Less(ts) {
		s.localResolvedTs = ts
	}

	var gcKeys []cloudStorageSinkKey
	for key, file := range s.files {
		// Any files where the bucket begin is `>= ts` don't need to be flushed
		// because of the Flush contract w.r.t. `ts`. (Bucket begin time is
		// exclusive and end time is inclusive).
		if !key.Bucket.Before(ts.GoTime()) {
			continue
		}

		// TODO(dan): These files should be further subdivided for three
		// reasons. 1) we could always gc anything we flush and later write a
		// followup bucket subdivion if needed 2) very large bucket sizes could
		// mean very large files, which are unwieldy once written 3) smooth
		// and/or control memory usage of the sink.
		filename := key.Filename()
		if log.V(1) {
			log.Info(ctx, "writing ", filename)
		}
		if err := s.writeFile(ctx, filename, file); err != nil {
			return err
		}

		// If the bucket end is `<= ts`, we'll never see another _previously
		// unseen_ row for this bucket. We drop any future such rows so that it
		// can be cleaned up.
		if end := key.Bucket.Add(s.bucketSize); ts.GoTime().After(end) {
			gcKeys = append(gcKeys, key)
		} else {
			if log.V(2) {
				log.Infof(ctx, "wrote %s but was not eligible for gc", filename)
			}
		}
	}
	for _, key := range gcKeys {
		delete(s.files, key)
	}

	return nil
}

func (s *cloudStorageSink) writeFile(
	ctx context.Context, name string, contents *bytes.Buffer,
) error {
	u := *s.base
	u.Path = filepath.Join(u.Path, name)
	es, err := storageccl.ExportStorageFromURI(ctx, u.String(), s.settings)
	if err != nil {
		return err
	}
	defer func() {
		if err := es.Close(); err != nil {
			log.Warningf(ctx, `failed to close %s, resources may have leaked: %s`, name, err)
		}
	}()
	r := bytes.NewReader(contents.Bytes())
	return es.WriteFile(ctx, ``, r)
}

// Close implements the Sink interface.
func (s *cloudStorageSink) Close() error {
	s.files = nil
	return nil
}

// causer matches the (unexported) interface used by Go to allow errors to wrap
// their parent cause.
type causer interface {
	Cause() error
}

// String and regex used to match retryable sink errors when they have been
// "flattened" into a pgerror.
const retryableSinkErrorString = "retryable sink error"

// retryableSinkError should be used by sinks to wrap any error which may
// be retried.
type retryableSinkError struct {
	cause error
}

func (e retryableSinkError) Error() string {
	return fmt.Sprintf(retryableSinkErrorString+": %s", e.cause.Error())
}
func (e retryableSinkError) Cause() error { return e.cause }

// isRetryableSinkError returns true if the supplied error, or any of its parent
// causes, is a retryableSinkError.
func isRetryableSinkError(err error) bool {
	for {
		if _, ok := err.(*retryableSinkError); ok {
			return true
		}
		// TODO(mrtracy): This pathway, which occurs when the retryable error is
		// detected on a non-local node of the distsql flow, is only currently
		// being tested with a roachtest, which is expensive. See if it can be
		// tested via a unit test,
		if _, ok := err.(*pgerror.Error); ok {
			return strings.Contains(err.Error(), retryableSinkErrorString)
		}
		if e, ok := err.(causer); ok {
			err = e.Cause()
			continue
		}
		return false
	}
}
