package metrics

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// SystemCollector reports VM-level resource metrics from /proc plus
// aggregate stats for the Chromium process tree.
type SystemCollector struct {
	procDir  string
	rootPath string
}

func NewSystemCollector() *SystemCollector {
	return &SystemCollector{procDir: "/proc", rootPath: "/"}
}

func (c *SystemCollector) Name() string { return "system" }

// userHZ is the kernel USER_HZ /proc/stat tick rate. Fixed at 100 on Linux
// for all architectures this image builds for.
const userHZ = 100

func (c *SystemCollector) Collect(ctx context.Context, w *Writer) error {
	if data, err := os.ReadFile(filepath.Join(c.procDir, "stat")); err == nil {
		if modes := parseProcStatCPU(string(data)); modes != nil {
			w.Metric("kernel_vm_cpu_seconds_total", "Cumulative CPU time by mode across all cores.", "counter")
			for _, m := range modes {
				w.Sample("kernel_vm_cpu_seconds_total", []Label{{"mode", m.mode}}, m.seconds)
			}
		}
	}

	if data, err := os.ReadFile(filepath.Join(c.procDir, "meminfo")); err == nil {
		mem := parseMeminfo(string(data))
		if v, ok := mem["MemTotal"]; ok {
			w.Metric("kernel_vm_memory_total_bytes", "Total VM memory.", "gauge")
			w.Sample("kernel_vm_memory_total_bytes", nil, v)
		}
		if v, ok := mem["MemAvailable"]; ok {
			w.Metric("kernel_vm_memory_available_bytes", "Estimated memory available for new workloads.", "gauge")
			w.Sample("kernel_vm_memory_available_bytes", nil, v)
		}
	}

	if data, err := os.ReadFile(filepath.Join(c.procDir, "loadavg")); err == nil {
		if fields := strings.Fields(string(data)); len(fields) > 0 {
			if v, err := strconv.ParseFloat(fields[0], 64); err == nil {
				w.Metric("kernel_vm_load1", "1-minute load average.", "gauge")
				w.Sample("kernel_vm_load1", nil, v)
			}
		}
	}

	if data, err := os.ReadFile(filepath.Join(c.procDir, "uptime")); err == nil {
		if fields := strings.Fields(string(data)); len(fields) > 0 {
			if v, err := strconv.ParseFloat(fields[0], 64); err == nil {
				// /proc/uptime is CLOCK_BOOTTIME-based, but an externally
				// paused VM's clocks stop entirely, so this still measures
				// active runtime rather than wall age.
				w.Metric("kernel_vm_uptime_seconds", "VM active runtime; excludes time spent suspended.", "gauge")
				w.Sample("kernel_vm_uptime_seconds", nil, v)
			}
		}
	}

	psi := false
	for _, resource := range []string{"cpu", "memory", "io"} {
		data, err := os.ReadFile(filepath.Join(c.procDir, "pressure", resource))
		if err != nil {
			continue
		}
		v, ok := parsePressureSomeAvg10(string(data))
		if !ok {
			continue
		}
		if !psi {
			w.Metric("kernel_vm_pressure_some_avg10_percent",
				"PSI: percentage of the last 10s in which at least one task stalled on the resource.", "gauge")
			psi = true
		}
		w.Sample("kernel_vm_pressure_some_avg10_percent", []Label{{"resource", resource}}, v)
	}

	if data, err := os.ReadFile(filepath.Join(c.procDir, "net", "dev")); err == nil {
		rx, tx := parseNetDev(string(data))
		w.Metric("kernel_vm_network_receive_bytes_total", "Bytes received on non-loopback interfaces.", "counter")
		w.Sample("kernel_vm_network_receive_bytes_total", nil, rx)
		w.Metric("kernel_vm_network_transmit_bytes_total", "Bytes transmitted on non-loopback interfaces.", "counter")
		w.Sample("kernel_vm_network_transmit_bytes_total", nil, tx)
	}

	var fs syscall.Statfs_t
	if err := syscall.Statfs(c.rootPath, &fs); err == nil {
		bsize := float64(fs.Bsize)
		w.Metric("kernel_vm_disk_total_bytes", "Filesystem size of the root mount.", "gauge")
		w.Sample("kernel_vm_disk_total_bytes", []Label{{"mount", c.rootPath}}, float64(fs.Blocks)*bsize)
		w.Metric("kernel_vm_disk_free_bytes", "Filesystem space available to unprivileged users on the root mount.", "gauge")
		w.Sample("kernel_vm_disk_free_bytes", []Label{{"mount", c.rootPath}}, float64(fs.Bavail)*bsize)
	}

	procs, rssBytes := c.chromiumProcesses()
	w.Metric("kernel_chromium_processes", "Number of running Chromium processes (browser, renderers, utilities).", "gauge")
	w.Sample("kernel_chromium_processes", nil, float64(procs))
	w.Metric("kernel_chromium_memory_rss_bytes", "Total resident memory of all Chromium processes.", "gauge")
	w.Sample("kernel_chromium_memory_rss_bytes", nil, rssBytes)

	return nil
}

type cpuMode struct {
	mode    string
	seconds float64
}

// parseProcStatCPU reads the aggregate "cpu" line of /proc/stat. Fields:
// user nice system idle iowait irq softirq steal. nice folds into user;
// irq and softirq fold into system.
func parseProcStatCPU(data string) []cpuMode {
	for _, line := range strings.Split(data, "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:]
		if len(fields) < 8 {
			return nil
		}
		ticks := make([]float64, 8)
		for i := range ticks {
			v, err := strconv.ParseFloat(fields[i], 64)
			if err != nil {
				return nil
			}
			ticks[i] = v
		}
		return []cpuMode{
			{"user", (ticks[0] + ticks[1]) / userHZ},
			{"system", (ticks[2] + ticks[5] + ticks[6]) / userHZ},
			{"idle", ticks[3] / userHZ},
			{"iowait", ticks[4] / userHZ},
			{"steal", ticks[7] / userHZ},
		}
	}
	return nil
}

// parseMeminfo returns /proc/meminfo values in bytes.
func parseMeminfo(data string) map[string]float64 {
	out := make(map[string]float64)
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			continue
		}
		out[strings.TrimSuffix(fields[0], ":")] = v * 1024
	}
	return out
}

// parseNetDev sums received and transmitted bytes across all non-loopback
// interfaces in /proc/net/dev.
func parseNetDev(data string) (rx, tx float64) {
	for _, line := range strings.Split(data, "\n") {
		name, stats, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(name) == "lo" {
			continue
		}
		fields := strings.Fields(stats)
		if len(fields) < 9 {
			continue
		}
		if v, err := strconv.ParseFloat(fields[0], 64); err == nil {
			rx += v
		}
		if v, err := strconv.ParseFloat(fields[8], 64); err == nil {
			tx += v
		}
	}
	return rx, tx
}

// parsePressureSomeAvg10 extracts the avg10 value from the "some" line of
// a /proc/pressure file, e.g. "some avg10=1.23 avg60=... total=...".
func parsePressureSomeAvg10(data string) (float64, bool) {
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "some" {
			continue
		}
		for _, f := range fields[1:] {
			if val, ok := strings.CutPrefix(f, "avg10="); ok {
				v, err := strconv.ParseFloat(val, 64)
				return v, err == nil
			}
		}
	}
	return 0, false
}

// chromiumProcesses counts processes whose comm is "chromium" and sums
// their resident memory. Summing per-process RSS double-counts shared
// pages, so treat the total as an upper bound useful for trends rather
// than absolute usage.
func (c *SystemCollector) chromiumProcesses() (count int, rssBytes float64) {
	entries, err := os.ReadDir(c.procDir)
	if err != nil {
		return 0, 0
	}
	pageSize := float64(os.Getpagesize())
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		comm, err := os.ReadFile(filepath.Join(c.procDir, e.Name(), "comm"))
		if err != nil || strings.TrimSpace(string(comm)) != "chromium" {
			continue
		}
		count++
		statm, err := os.ReadFile(filepath.Join(c.procDir, e.Name(), "statm"))
		if err != nil {
			continue
		}
		fields := strings.Fields(string(statm))
		if len(fields) < 2 {
			continue
		}
		if pages, err := strconv.ParseFloat(fields[1], 64); err == nil {
			rssBytes += pages * pageSize
		}
	}
	return count, rssBytes
}
