package metrics

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Trimmed from real `nvidia-smi -q -x` output on a vGPU browser VM.
const smiLicensed = `<?xml version="1.0" ?>
<nvidia_smi_log>
	<driver_version>580.105.08</driver_version>
	<attached_gpus>1</attached_gpus>
	<gpu id="00000000:00:07.0">
		<product_name>NVIDIA L40S-2Q</product_name>
		<vgpu_software_licensed_product>
			<licensed_product_name>NVIDIA RTX Virtual Workstation</licensed_product_name>
			<license_status>Licensed (Expiry: 2026-7-7 16:1:44 GMT)</license_status>
		</vgpu_software_licensed_product>
		<fb_memory_usage>
			<total>2048 MiB</total>
			<reserved>458 MiB</reserved>
			<used>406 MiB</used>
			<free>1183 MiB</free>
		</fb_memory_usage>
		<utilization>
			<gpu_util>7 %</gpu_util>
			<memory_util>2 %</memory_util>
			<encoder_util>0 %</encoder_util>
			<decoder_util>N/A</decoder_util>
		</utilization>
	</gpu>
</nvidia_smi_log>`

func TestParseNvidiaSMILicensed(t *testing.T) {
	stats, err := parseNvidiaSMI([]byte(smiLicensed))
	require.NoError(t, err)

	assert.Equal(t, "NVIDIA L40S-2Q", stats.Product)
	assert.Equal(t, "580.105.08", stats.DriverVersion)
	assert.Equal(t, "NVIDIA RTX Virtual Workstation", stats.LicensedProduct)
	assert.True(t, stats.Licensed)
	require.NotNil(t, stats.LicenseExpiry)
	assert.Equal(t, time.Date(2026, 7, 7, 16, 1, 44, 0, time.UTC), stats.LicenseExpiry.UTC())

	require.NotNil(t, stats.GPUUtil)
	assert.Equal(t, 7.0, *stats.GPUUtil)
	require.NotNil(t, stats.EncoderUtil)
	assert.Equal(t, 0.0, *stats.EncoderUtil)
	assert.Nil(t, stats.DecoderUtil)

	require.NotNil(t, stats.MemoryUsedBytes)
	assert.Equal(t, 406.0*1024*1024, *stats.MemoryUsedBytes)
	require.NotNil(t, stats.MemoryTotalBytes)
	assert.Equal(t, 2048.0*1024*1024, *stats.MemoryTotalBytes)
}

func TestParseNvidiaSMIUnlicensed(t *testing.T) {
	xml := strings.Replace(smiLicensed,
		"Licensed (Expiry: 2026-7-7 16:1:44 GMT)", "Unlicensed (Restricted)", 1)
	stats, err := parseNvidiaSMI([]byte(xml))
	require.NoError(t, err)
	assert.False(t, stats.Licensed)
	assert.Nil(t, stats.LicenseExpiry)
}

func TestParseNvidiaSMIUnknownZone(t *testing.T) {
	// A non-GMT zone must fail loudly (no expiry) instead of silently
	// parsing with a zero offset.
	xml := strings.Replace(smiLicensed,
		"Expiry: 2026-7-7 16:1:44 GMT", "Expiry: 2026-7-7 16:1:44 PST", 1)
	stats, err := parseNvidiaSMI([]byte(xml))
	require.NoError(t, err)
	assert.True(t, stats.Licensed)
	assert.Nil(t, stats.LicenseExpiry)
}

func TestGPUCollectorNoGPU(t *testing.T) {
	c := NewGPUCollector()
	c.devicesDir = t.TempDir() // exists but empty

	w := &Writer{}
	require.NoError(t, c.Collect(context.Background(), w))
	assert.Contains(t, string(w.Bytes()), "kernel_gpu_present 0\n")
	assert.NotContains(t, string(w.Bytes()), "kernel_gpu_licensed")
}

func TestGPUCollectorLicensed(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "0000:00:07.0"), 0o755))

	c := NewGPUCollector()
	c.devicesDir = dir
	c.querySMI = func(context.Context) ([]byte, error) { return []byte(smiLicensed), nil }
	c.now = func() time.Time { return time.Date(2026, 7, 7, 15, 56, 44, 0, time.UTC) }

	w := &Writer{}
	require.NoError(t, c.Collect(context.Background(), w))
	out := string(w.Bytes())

	assert.Contains(t, out, "kernel_gpu_present 1\n")
	assert.Contains(t, out, `kernel_gpu_info{product="NVIDIA L40S-2Q",licensed_product="NVIDIA RTX Virtual Workstation",driver_version="580.105.08"} 1`)
	assert.Contains(t, out, "kernel_gpu_licensed 1\n")
	assert.Contains(t, out, "kernel_gpu_license_expiry_seconds 300\n")
	assert.Contains(t, out, "kernel_gpu_utilization_percent 7\n")
	assert.Contains(t, out, "kernel_gpu_memory_used_bytes 4.25721856e+08\n")
	assert.NotContains(t, out, "kernel_gpu_decoder_utilization_percent")
}
