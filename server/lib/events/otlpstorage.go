package events

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
)

// defaultOTLPMaxBatchRecords bounds records per export request. Envelopes are
// capped at ~1MB each at publish, so a small batch keeps typical requests
// under a common 10MB body cap. This is a count bound, not a byte bound:
// byte-based batching (the real guard against many large records) is deferred.
const defaultOTLPMaxBatchRecords = 200

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
}

// loggingExporter wraps the OTLP exporter to surface export-time failures
// (network / customer-endpoint errors), which the SDK otherwise reports only
// through the global otel logger. Queue-overflow drops under sustained
// backpressure are best-effort: the SDK drops the oldest record, bounded by the
// batch queue size, and are not counted here.
type loggingExporter struct {
	sdklog.Exporter
	log      *slog.Logger
	failures atomic.Uint64
}

func (e *loggingExporter) Export(ctx context.Context, records []sdklog.Record) error {
	err := e.Exporter.Export(ctx, records)
	if err != nil {
		n := e.failures.Add(1)
		e.log.Warn("otlp export failed", "records", len(records), "err", err, "total_failures", n)
	}
	return err
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

	processor := sdklog.NewBatchProcessor(
		&loggingExporter{Exporter: exporter, log: log},
		sdklog.WithExportMaxBatchSize(defaultOTLPMaxBatchRecords),
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
// Stop to drain and shut down. Mirrors S2StorageWriter.
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
	w.writer = NewStorageWriter(w.es, storage, w.log)
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
		return ctx.Err()
	}
	if err := w.writer.Drain(ctx); err != nil {
		w.log.Warn("otlp storage writer: drain incomplete", "err", err)
	}
	return w.storage.Close(ctx)
}
