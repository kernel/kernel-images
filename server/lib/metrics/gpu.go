package metrics

import (
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// GPUCollector reports GPU presence and, when a GPU is attached, vGPU
// license state and utilization read from nvidia-smi. On VMs without the
// NVIDIA driver it emits only kernel_gpu_present 0.
type GPUCollector struct {
	devicesDir string
	querySMI   func(ctx context.Context) ([]byte, error)
	now        func() time.Time
}

func NewGPUCollector() *GPUCollector {
	return &GPUCollector{
		devicesDir: "/proc/driver/nvidia/gpus",
		querySMI: func(ctx context.Context) ([]byte, error) {
			return exec.CommandContext(ctx, "nvidia-smi", "-q", "-x").Output()
		},
		now: time.Now,
	}
}

func (c *GPUCollector) Name() string { return "gpu" }

func (c *GPUCollector) Collect(ctx context.Context, w *Writer) error {
	w.Metric("kernel_gpu_present", "Whether an NVIDIA GPU is attached to this VM.", "gauge")
	entries, err := os.ReadDir(c.devicesDir)
	if err != nil || len(entries) == 0 {
		w.Sample("kernel_gpu_present", nil, 0)
		return nil
	}
	w.Sample("kernel_gpu_present", nil, 1)

	out, err := c.querySMI(ctx)
	if err != nil {
		return fmt.Errorf("nvidia-smi: %w", err)
	}
	stats, err := parseNvidiaSMI(out)
	if err != nil {
		return err
	}

	w.Metric("kernel_gpu_info", "GPU device and vGPU license info; value is always 1.", "gauge")
	w.Sample("kernel_gpu_info", []Label{
		{"product", stats.Product},
		{"licensed_product", stats.LicensedProduct},
		{"driver_version", stats.DriverVersion},
	}, 1)

	w.Metric("kernel_gpu_licensed", "Whether the vGPU software license is active.", "gauge")
	w.Sample("kernel_gpu_licensed", nil, boolToFloat(stats.Licensed))

	if stats.LicenseExpiry != nil {
		w.Metric("kernel_gpu_license_expiry_seconds",
			"Seconds until the vGPU license lease expires. The license daemon renews the lease continuously, so a value trending to zero means renewal is failing.",
			"gauge")
		w.Sample("kernel_gpu_license_expiry_seconds", nil, stats.LicenseExpiry.Sub(c.now()).Seconds())
	}

	utilization := []struct {
		name string
		val  *float64
	}{
		{"kernel_gpu_utilization_percent", stats.GPUUtil},
		{"kernel_gpu_memory_utilization_percent", stats.MemoryUtil},
		{"kernel_gpu_encoder_utilization_percent", stats.EncoderUtil},
		{"kernel_gpu_decoder_utilization_percent", stats.DecoderUtil},
	}
	for _, u := range utilization {
		if u.val != nil {
			w.Metric(u.name, "GPU utilization reported by nvidia-smi.", "gauge")
			w.Sample(u.name, nil, *u.val)
		}
	}
	if stats.MemoryUsedBytes != nil {
		w.Metric("kernel_gpu_memory_used_bytes", "GPU framebuffer memory in use.", "gauge")
		w.Sample("kernel_gpu_memory_used_bytes", nil, *stats.MemoryUsedBytes)
	}
	if stats.MemoryTotalBytes != nil {
		w.Metric("kernel_gpu_memory_total_bytes", "Total GPU framebuffer memory.", "gauge")
		w.Sample("kernel_gpu_memory_total_bytes", nil, *stats.MemoryTotalBytes)
	}
	return nil
}

type gpuStats struct {
	Product          string
	DriverVersion    string
	LicensedProduct  string
	Licensed         bool
	LicenseExpiry    *time.Time
	GPUUtil          *float64
	MemoryUtil       *float64
	EncoderUtil      *float64
	DecoderUtil      *float64
	MemoryUsedBytes  *float64
	MemoryTotalBytes *float64
}

type smiLog struct {
	DriverVersion string `xml:"driver_version"`
	GPUs          []struct {
		ProductName string `xml:"product_name"`
		Licensed    struct {
			ProductName string `xml:"licensed_product_name"`
			Status      string `xml:"license_status"`
		} `xml:"vgpu_software_licensed_product"`
		FBMemory struct {
			Total string `xml:"total"`
			Used  string `xml:"used"`
		} `xml:"fb_memory_usage"`
		Utilization struct {
			GPU     string `xml:"gpu_util"`
			Memory  string `xml:"memory_util"`
			Encoder string `xml:"encoder_util"`
			Decoder string `xml:"decoder_util"`
		} `xml:"utilization"`
	} `xml:"gpu"`
}

// licenseExpiryRe matches the expiry timestamp inside a license status like
// "Licensed (Expiry: 2026-7-7 15:58:42 GMT)". nvidia-smi does not zero-pad
// date or time components. The zone is captured separately and required to
// be GMT: time.Parse would silently give an unknown abbreviation a zero
// offset, so a driver that starts emitting another zone must fail parsing
// loudly (no expiry metric) rather than skew it.
var licenseExpiryRe = regexp.MustCompile(`Expiry: (\d{4}-\d{1,2}-\d{1,2} \d{1,2}:\d{1,2}:\d{1,2}) (\w+)`)

func parseNvidiaSMI(out []byte) (*gpuStats, error) {
	var log smiLog
	if err := xml.Unmarshal(out, &log); err != nil {
		return nil, fmt.Errorf("parse nvidia-smi xml: %w", err)
	}
	if len(log.GPUs) == 0 {
		return nil, fmt.Errorf("nvidia-smi reported no GPUs")
	}
	// Browser VMs get exactly one vGPU slice; only the first GPU is read.
	gpu := log.GPUs[0]

	stats := &gpuStats{
		Product:         gpu.ProductName,
		DriverVersion:   log.DriverVersion,
		LicensedProduct: gpu.Licensed.ProductName,
		Licensed:        strings.HasPrefix(gpu.Licensed.Status, "Licensed"),
	}
	if m := licenseExpiryRe.FindStringSubmatch(gpu.Licensed.Status); m != nil && m[2] == "GMT" {
		if t, err := time.ParseInLocation("2006-1-2 15:4:5", m[1], time.UTC); err == nil {
			stats.LicenseExpiry = &t
		}
	}
	stats.GPUUtil = parsePercent(gpu.Utilization.GPU)
	stats.MemoryUtil = parsePercent(gpu.Utilization.Memory)
	stats.EncoderUtil = parsePercent(gpu.Utilization.Encoder)
	stats.DecoderUtil = parsePercent(gpu.Utilization.Decoder)
	stats.MemoryUsedBytes = parseMiB(gpu.FBMemory.Used)
	stats.MemoryTotalBytes = parseMiB(gpu.FBMemory.Total)
	return stats, nil
}

// parsePercent parses nvidia-smi values like "42 %". Returns nil for
// missing or "N/A" values.
func parsePercent(s string) *float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "%")), 64)
	if err != nil {
		return nil
	}
	return &v
}

// parseMiB parses nvidia-smi memory values like "406 MiB" into bytes.
// Returns nil for missing or "N/A" values.
func parseMiB(s string) *float64 {
	fields := strings.Fields(s)
	if len(fields) != 2 || fields[1] != "MiB" {
		return nil
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return nil
	}
	v *= 1024 * 1024
	return &v
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
