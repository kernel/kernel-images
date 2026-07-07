package metrics

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseProcStatCPU(t *testing.T) {
	data := `cpu  1000 200 300 40000 500 60 70 80 0 0
cpu0 500 100 150 20000 250 30 35 40 0 0
`
	modes := parseProcStatCPU(data)
	require.Len(t, modes, 5)
	byMode := map[string]float64{}
	for _, m := range modes {
		byMode[m.mode] = m.seconds
	}
	assert.Equal(t, 12.0, byMode["user"])  // (1000+200)/100
	assert.Equal(t, 4.3, byMode["system"]) // (300+60+70)/100
	assert.Equal(t, 400.0, byMode["idle"]) // 40000/100
	assert.Equal(t, 5.0, byMode["iowait"]) // 500/100
	assert.Equal(t, 0.8, byMode["steal"])  // 80/100
}

func TestParseMeminfo(t *testing.T) {
	mem := parseMeminfo("MemTotal:        4014828 kB\nMemAvailable:    2534512 kB\nHugePages_Total:       0\n")
	assert.Equal(t, 4014828.0*1024, mem["MemTotal"])
	assert.Equal(t, 2534512.0*1024, mem["MemAvailable"])
}

func TestParseNetDev(t *testing.T) {
	data := `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo:  999999    1000    0    0    0     0          0         0   999999    1000    0    0    0     0       0          0
  eth0: 1500000   12000    0    0    0     0          0         0   300000    8000    0    0    0     0       0          0
  eth1:  500000    2000    0    0    0     0          0         0   200000    1000    0    0    0     0       0          0
`
	rx, tx := parseNetDev(data)
	assert.Equal(t, 2000000.0, rx)
	assert.Equal(t, 500000.0, tx)
}

func TestChromiumProcesses(t *testing.T) {
	proc := t.TempDir()
	mkProc := func(pid, comm, statm string) {
		dir := filepath.Join(proc, pid)
		require.NoError(t, os.Mkdir(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "comm"), []byte(comm+"\n"), 0o644))
		if statm != "" {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "statm"), []byte(statm), 0o644))
		}
	}
	mkProc("100", "chromium", "5000 1000 300 10 0 500 0")
	mkProc("101", "chromium", "6000 2000 300 10 0 500 0")
	mkProc("102", "chrome_crashpad", "100 50 30 1 0 5 0")
	mkProc("103", "bash", "100 50 30 1 0 5 0")

	c := &SystemCollector{procDir: proc, rootPath: "/"}
	count, rss := c.chromiumProcesses()
	assert.Equal(t, 2, count)
	assert.Equal(t, 3000.0*float64(os.Getpagesize()), rss)
}
