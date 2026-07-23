package events

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
)

// DefaultOTLPMaxBatchRecords bounds how many records the SDK buffers per export
// cycle. The per-request byte size is bounded separately by the loggingExporter,
// which splits an export into sub-requests under maxOTLPExportBytes so a batch of
// large records can't exceed the target's HTTP body limit. Exported so the
// config layer can validate the queue size against it (the queue must hold at
// least one full batch).
const DefaultOTLPMaxBatchRecords = 200

// maxOTLPExportBytes caps the estimated payload of a single export request so it
// stays well under common OTLP/HTTP body limits (collectors default to ~20MiB).
// A single record is at most ~1MB (the publish-time envelope cap), so every
// record fits within one sub-request.
const maxOTLPExportBytes = 4 << 20 // 4 MiB

// otlpForceCloseTimeout bounds the provider shutdown when Stop's read-loop wait
// has already exhausted its ctx, so a hung flush can't block indefinitely.
const otlpForceCloseTimeout = 2 * time.Second

// OTLPConfig configures the OTLP telemetry sink.
type OTLPConfig struct {
	// Endpoint is the host[:port] of the OTLP/HTTP target: a collector in
	// development, or a forwarding relay in production.
	Endpoint string
	// URLPath overrides the request path. Empty uses the exporter default of
	// /v1/logs.
	URLPath string
	// Insecure sends over plaintext HTTP. Development only.
	Insecure bool
	// Headers are attached to every export request (e.g. the instance JWT).
	Headers map[string]string

	// ServiceName, InstanceName, and Metro populate the OTLP Resource.
	ServiceName  string
	InstanceName string
	Metro        string

	// Batch tuning. Zero values fall back to the SDK defaults.
	MaxQueueSize   int
	ExportInterval time.Duration
	ExportTimeout  time.Duration

	// Metrics accumulates export counters. When nil a throwaway is used so the
	// sink still runs; callers that scrape metrics pass a shared instance.
	Metrics *OTLPMetrics
}

// loggingExporter wraps the OTLP exporter to (1) split each export into
// sub-requests under maxOTLPExportBytes so a batch of large records can't exceed
// the target's HTTP body limit, and (2) record export outcomes into the shared
// OTLPMetrics (failures and successfully exported records). Queue drops under
// sustained backpressure are counted separately, via the SDK's global logger,
// which main wires through NewDropCountingHandler.
type loggingExporter struct {
	sdklog.Exporter
	log     *slog.Logger
	metrics *OTLPMetrics
}

func (e *loggingExporter) Export(ctx context.Context, records []sdklog.Record) error {
	var firstErr error
	for _, chunk := range chunkBySize(records, maxOTLPExportBytes) {
		if err := e.Exporter.Export(ctx, chunk); err != nil {
			n := e.metrics.failures.Add(1)
			e.log.Warn("otlp export failed", "records", len(chunk), "err", err, "total_failures", n)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		e.metrics.exported.Add(uint64(len(chunk)))
	}
	return firstErr
}

// chunkBySize splits records into consecutive groups whose estimated encoded
// size stays under maxBytes, so no single export request exceeds the target's
// body limit. A record larger than maxBytes on its own still ships alone.
func chunkBySize(records []sdklog.Record, maxBytes int) [][]sdklog.Record {
	if len(records) == 0 {
		return nil
	}
	var chunks [][]sdklog.Record
	start, size := 0, 0
	for i := range records {
		rs := estimateRecordBytes(&records[i])
		if i > start && size+rs > maxBytes {
			chunks = append(chunks, records[start:i])
			start, size = i, 0
		}
		size += rs
	}
	return append(chunks, records[start:])
}

// estimateRecordBytes approximates a record's encoded size from its body and
// attributes. It ignores protobuf framing, which the maxOTLPExportBytes headroom
// absorbs.
func estimateRecordBytes(r *sdklog.Record) int {
	n := estimateValueBytes(r.Body())
	r.WalkAttributes(func(kv log.KeyValue) bool {
		n += len(kv.Key) + estimateValueBytes(kv.Value)
		return true
	})
	return n
}

func estimateValueBytes(v log.Value) int {
	switch v.Kind() {
	case log.KindString:
		return len(v.AsString())
	case log.KindBytes:
		return len(v.AsBytes())
	case log.KindSlice:
		n := 0
		for _, e := range v.AsSlice() {
			n += estimateValueBytes(e)
		}
		return n
	case log.KindMap:
		n := 0
		for _, kv := range v.AsMap() {
			n += len(kv.Key) + estimateValueBytes(kv.Value)
		}
		return n
	default:
		return 8 // bool / int64 / float64 / empty: fixed-width
	}
}

// otlpStorage implements Storage by converting envelopes to OTLP log records
// and emitting them through the OTel log SDK, which batches and exports to the
// configured endpoint.
type otlpStorage struct {
	provider *sdklog.LoggerProvider
	logger   log.Logger
}

func newOTLPStorage(ctx context.Context, cfg OTLPConfig, log *slog.Logger) (*otlpStorage, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("otlp storage: endpoint is required")
	}

	opts := []otlploghttp.Option{otlploghttp.WithEndpoint(cfg.Endpoint)}
	if cfg.URLPath != "" {
		opts = append(opts, otlploghttp.WithURLPath(cfg.URLPath))
	}
	if cfg.Insecure {
		opts = append(opts, otlploghttp.WithInsecure())
	}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlploghttp.WithHeaders(cfg.Headers))
	}

	exporter, err := otlploghttp.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("otlp storage: new exporter: %w", err)
	}

	attrs := []attribute.KeyValue{semconv.ServiceName(cfg.ServiceName)}
	if cfg.InstanceName != "" {
		attrs = append(attrs, attribute.String("kernel.instance_name", cfg.InstanceName))
	}
	if cfg.Metro != "" {
		attrs = append(attrs, attribute.String("kernel.metro", cfg.Metro))
	}
	res := resource.NewSchemaless(attrs...)

	procOpts := []sdklog.BatchProcessorOption{sdklog.WithExportMaxBatchSize(DefaultOTLPMaxBatchRecords)}
	if cfg.MaxQueueSize > 0 {
		procOpts = append(procOpts, sdklog.WithMaxQueueSize(cfg.MaxQueueSize))
	}
	if cfg.ExportInterval > 0 {
		procOpts = append(procOpts, sdklog.WithExportInterval(cfg.ExportInterval))
	}
	if cfg.ExportTimeout > 0 {
		procOpts = append(procOpts, sdklog.WithExportTimeout(cfg.ExportTimeout))
	}
	metrics := cfg.Metrics
	if metrics == nil {
		metrics = &OTLPMetrics{}
	}
	processor := sdklog.NewBatchProcessor(
		&loggingExporter{Exporter: exporter, log: log, metrics: metrics},
		procOpts...,
	)
	provider := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(processor),
	)

	return &otlpStorage{
		provider: provider,
		logger:   provider.Logger(otlpScopeName),
	}, nil
}

// Append converts an exported-category envelope to an OTLP record and hands it
// to the SDK. Excluded categories are dropped. Delivery failures surface via
// the loggingExporter, not this return value (the SDK batches asynchronously).
func (s *otlpStorage) Append(ctx context.Context, env Envelope) error {
	if !otlpCategoryExported(env.Event.Category) {
		return nil
	}
	s.logger.Emit(ctx, toLogRecord(env))
	return nil
}

// Close flushes buffered records and shuts down the exporter.
func (s *otlpStorage) Close(ctx context.Context) error {
	return s.provider.Shutdown(ctx)
}

// OTLPStorageWriter reads from an EventStream and forwards each event to an
// OTLP endpoint. Construct with NewOTLPStorageWriter, call Start to begin and
// Stop to drain and shut down. Unlike S2StorageWriter it starts from the stream
// tail at Start, so a writer rebuilt on a runtime export re-enable forwards only
// new events rather than replaying the retained ring.
type OTLPStorageWriter struct {
	es  *EventStream
	cfg OTLPConfig
	log *slog.Logger

	mu      sync.Mutex
	started bool
	storage *otlpStorage
	writer  *StorageWriter
	done    chan struct{}
}

func NewOTLPStorageWriter(es *EventStream, cfg OTLPConfig, log *slog.Logger) *OTLPStorageWriter {
	return &OTLPStorageWriter{es: es, cfg: cfg, log: log}
}

// Start builds the exporter and begins reading from the event stream. ctx
// governs the Run loop; cancel it (e.g. on SIGTERM) to stop reading. The
// exporter outlives ctx and is torn down by Stop after flushing.
func (w *OTLPStorageWriter) Start(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started {
		return fmt.Errorf("otlpstoragewriter: Start called more than once")
	}
	storage, err := newOTLPStorage(ctx, w.cfg, w.log)
	if err != nil {
		return err
	}
	w.storage = storage
	// Start from the current tail, not seq 0: export is toggled at runtime and
	// the writer is rebuilt on each enable, so reading from the oldest buffered
	// event would re-export the retained ring every time it is turned back on.
	// Only events published while export is on are forwarded.
	w.writer = NewStorageWriterAfter(w.es, storage, w.log, w.es.Seq())
	w.done = make(chan struct{})
	w.started = true
	go func() {
		defer close(w.done)
		if err := w.writer.Run(ctx); err != nil && ctx.Err() == nil {
			w.log.Error("otlp storage writer failed", "err", err)
		}
	}()
	return nil
}

// Stop waits for the Run goroutine to exit, drains remaining ring events, then
// flushes and shuts down the exporter. ctx bounds total shutdown time.
func (w *OTLPStorageWriter) Stop(ctx context.Context) error {
	w.mu.Lock()
	if !w.started {
		w.mu.Unlock()
		return nil
	}
	w.mu.Unlock()

	select {
	case <-w.done:
	case <-ctx.Done():
		// The read loop didn't exit in time. Still shut the provider down so its
		// batch-export goroutines don't leak; detach from the expired ctx to give
		// Shutdown a brief window to flush.
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), otlpForceCloseTimeout)
		defer cancel()
		_ = w.storage.Close(closeCtx)
		return ctx.Err()
	}
	if err := w.writer.Drain(ctx); err != nil {
		w.log.Warn("otlp storage writer: drain incomplete", "err", err)
	}
	return w.storage.Close(ctx)
}

// OTLPExportController starts and stops OTLP export on demand so it can be
// toggled at runtime via the telemetry API. The underlying writer is one-shot,
// so each enable builds a fresh one. Safe for concurrent use.
type OTLPExportController struct {
	es  *EventStream
	cfg OTLPConfig
	log *slog.Logger

	mu     sync.Mutex
	writer *OTLPStorageWriter
	cancel context.CancelFunc
}

func NewOTLPExportController(es *EventStream, cfg OTLPConfig, log *slog.Logger) *OTLPExportController {
	return &OTLPExportController{es: es, cfg: cfg, log: log}
}

// Start begins export, or is a no-op if already running. parent governs the
// read loop; the controller derives a cancelable child so Stop can halt the
// loop even when parent is still live (a runtime toggle-off).
func (c *OTLPExportController) Start(parent context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.writer != nil {
		return nil
	}
	runCtx, cancel := context.WithCancel(parent)
	w := NewOTLPStorageWriter(c.es, c.cfg, c.log)
	if err := w.Start(runCtx); err != nil {
		cancel()
		return err
	}
	c.writer, c.cancel = w, cancel
	return nil
}

// Stop drains and shuts down a running exporter, or is a no-op if stopped. ctx
// bounds shutdown time.
func (c *OTLPExportController) Stop(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.writer == nil {
		return nil
	}
	c.cancel()
	err := c.writer.Stop(ctx)
	c.writer, c.cancel = nil, nil
	return err
}

// Running reports whether export is currently active.
func (c *OTLPExportController) Running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writer != nil
}
