package sysmon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// canonicalOomDump is a representative slice of the kmsg lines the kernel
// emits during a global OOM kill on Linux 5.x. The Mem-Info and Tasks
// state sections are abbreviated but preserve the field layout the
// parser depends on.
var canonicalOomDump = []string{
	`chromium invoked oom-killer: gfp_mask=0x100cca(GFP_HIGHUSER_MOVABLE), order=0, oom_score_adj=0`,
	`CPU: 2 PID: 1234 Comm: chromium Not tainted 5.15.0-1-amd64 #1`,
	`Call Trace:`,
	` dump_stack_lvl+0x44/0x57`,
	`Mem-Info:`,
	`active_anon:123456 inactive_anon:78901 isolated_anon:0`,
	` slab_reclaimable:2340 slab_unreclaimable:5670`,
	` mapped:8901 shmem:120 pagetables:340`,
	` free:4560 free_pcp:0 free_cma:0`,
	`Node 0 active_anon:493824kB inactive_anon:315604kB`,
	`Node 0 DMA free:11264kB boost:0kB min:64kB`,
	`Node 0 DMA32 free:6976kB boost:0kB min:8120kB`,
	`524288 pages RAM`,
	`0 pages HighMem/MovableOnly`,
	`21465 pages reserved`,
	`Tasks state (memory values in pages):`,
	`[  pid  ]   uid  tgid total_vm      rss pgtables_bytes swapents oom_score_adj name`,
	`[    111]     0   111     1234      234      45056        0          -250 systemd`,
	`[    234]     0   234    65432    12345     200704        0             0 sshd`,
	`[   1234]  1000  1234  1308611  1205975    9678848        0             0 chromium`,
	`[   5678]  1000  5678   123456    34567     331776        0             0 mutter`,
	`oom-kill:constraint=CONSTRAINT_NONE,nodemask=(null),cpuset=/,mems_allowed=0,global_oom,task_memcg=/,task=chromium,pid=1234,uid=1000`,
	`Out of memory: Killed process 1234 (chromium) total-vm:5234572kB, anon-rss:4823900kB, file-rss:100kB, shmem-rss:200kB, UID:1000 pgtables:9678848kB oom_score_adj:0`,
}

// feedAll runs every line in dump through the scanner and returns every
// completed OomInstance, in order. Used by tests that need to verify a
// whole dump's parsing.
func feedAll(s *oomScanner, dump []string, base time.Time) []OomInstance {
	var out []OomInstance
	for i, line := range dump {
		ts := base.Add(time.Duration(i) * time.Millisecond)
		if oom := s.feed(line, ts); oom != nil {
			out = append(out, *oom)
		}
	}
	return out
}

func TestOomScannerCanonicalDump(t *testing.T) {
	var s oomScanner
	base := time.Unix(1_700_000_000, 0)
	got := feedAll(&s, canonicalOomDump, base)
	require.Len(t, got, 1)

	oom := got[0]
	assert.Equal(t, "chromium", oom.ProcessName)
	assert.Equal(t, 1234, oom.Pid)
	assert.Equal(t, 4823900+100+200, oom.RssKb)
	assert.Equal(t, "none", oom.Constraint)

	// 524288 pages * 4 KiB = 2 GiB total
	assert.Equal(t, 524288*4, oom.MemTotalKb)
	// 4560 pages * 4 KiB = ~17.8 MiB free
	assert.Equal(t, 4560*4, oom.MemFreeKb)

	// Top tasks sorted by RSS desc; the 4 tasks in the fixture all fit
	// under the cap so the slice contains all of them in the right order.
	require.Len(t, oom.TopTasks, 4)
	assert.Equal(t, "chromium", oom.TopTasks[0].Name)
	assert.Equal(t, 1234, oom.TopTasks[0].Pid)
	assert.Equal(t, 1205975*4, oom.TopTasks[0].RssKb)
	assert.Equal(t, "mutter", oom.TopTasks[1].Name)
	assert.Equal(t, "sshd", oom.TopTasks[2].Name)
	assert.Equal(t, "systemd", oom.TopTasks[3].Name)
}

func TestOomScannerLegacyKernelNoMemInfoNoTasks(t *testing.T) {
	// Pre-5.0 kernels emit just the opening line, some stack trace, and
	// the closing line. The scanner must still produce a baseline event
	// with the new fields left at their zero values.
	dump := []string{
		`chromium invoked oom-killer: gfp_mask=0x14000c0, order=0, oom_score_adj=0`,
		`Mem-Info:`,
		`Out of memory: Killed process 9 (mutter) total-vm:200kB, anon-rss:150kB, file-rss:10kB, shmem-rss:5kB, UID:1000 pgtables:1kB oom_score_adj:0`,
	}
	var s oomScanner
	got := feedAll(&s, dump, time.Now())
	require.Len(t, got, 1)
	oom := got[0]
	assert.Equal(t, "mutter", oom.ProcessName)
	assert.Equal(t, 9, oom.Pid)
	assert.Equal(t, 165, oom.RssKb)
	assert.Empty(t, oom.Constraint)
	assert.Zero(t, oom.MemTotalKb)
	assert.Zero(t, oom.MemFreeKb)
	assert.Empty(t, oom.TopTasks)
}

func TestOomScannerTasksTableCappedAtTopN(t *testing.T) {
	// Build a synthetic Tasks state with more than topTasksN entries to
	// verify the scanner sorts by RSS and trims.
	dump := []string{`chromium invoked oom-killer: gfp_mask=0, order=0, oom_score_adj=0`}
	rssVals := []int{100, 900, 50, 800, 700, 200, 600, 300, 400, 500} // 10 procs
	for i, rss := range rssVals {
		dump = append(dump,
			"[   "+itoa(i+1)+"]     0   "+itoa(i+1)+"    1000      "+itoa(rss)+"     1024        0             0 proc"+itoa(i+1))
	}
	dump = append(dump,
		`Out of memory: Killed process 1 (proc1) total-vm:0kB, anon-rss:0kB, file-rss:0kB, shmem-rss:0kB, UID:0 pgtables:0kB oom_score_adj:0`,
	)

	var s oomScanner
	got := feedAll(&s, dump, time.Now())
	require.Len(t, got, 1)
	require.Len(t, got[0].TopTasks, topTasksN, "should cap at topTasksN")
	// Top 5 by RSS: 900, 800, 700, 600, 500
	wantRss := []int{900 * pageSizeKB, 800 * pageSizeKB, 700 * pageSizeKB, 600 * pageSizeKB, 500 * pageSizeKB}
	for i, w := range wantRss {
		assert.Equal(t, w, got[0].TopTasks[i].RssKb, "position %d", i)
	}
}

func TestOomScannerCommWithInternalSpace(t *testing.T) {
	var s oomScanner
	s.feed("invoked oom-killer:", time.Now())
	got := s.feed(`Out of memory: Killed process 42 (kworker u4:1) total-vm:0kB, anon-rss:0kB, file-rss:0kB, shmem-rss:0kB, UID:0 pgtables:0kB oom_score_adj:0`, time.Now())
	require.NotNil(t, got)
	assert.Equal(t, "kworker u4:1", got.ProcessName)
	assert.Equal(t, 42, got.Pid)
}

func TestOomScannerIgnoresPreambleWhenIdle(t *testing.T) {
	// A "Killed process" line in isolation must NOT emit. Without the
	// section delimiter we cannot attribute it reliably; the kernel
	// occasionally surfaces orphaned OOM lines when the ring buffer
	// wraps.
	var s oomScanner
	got := s.feed(`Out of memory: Killed process 1 (init) total-vm:1kB, anon-rss:0kB, file-rss:0kB, shmem-rss:0kB, UID:0 pgtables:0kB oom_score_adj:0`, time.Now())
	assert.Nil(t, got)
}

func TestOomScannerIgnoresUnrelatedLines(t *testing.T) {
	var s oomScanner
	assert.Nil(t, s.feed("usb 1-1: new high-speed USB device number 5", time.Now()))
	// A bracketed line that LOOKS like a task entry but has the wrong
	// column count should not crash or misparse.
	assert.Nil(t, s.feed("[1234] only one field", time.Now()))
}

func TestOomScannerSecondStartAbandonsFirst(t *testing.T) {
	// If a section never completes and a new one starts, the new section
	// must not inherit state from the abandoned one.
	var s oomScanner
	s.feed("invoked oom-killer:", time.Now())
	s.feed("oom-kill:constraint=CONSTRAINT_MEMCG,task=stale,pid=1,uid=0", time.Now())
	s.feed("[   1]     0     1     100     900     1024        0             0 stale", time.Now())
	s.feed("invoked oom-killer:", time.Now())
	got := s.feed(`Out of memory: Killed process 7 (real) total-vm:0kB, anon-rss:10kB, file-rss:0kB, shmem-rss:0kB, UID:0 pgtables:0kB oom_score_adj:0`, time.Now())
	require.NotNil(t, got)
	assert.Equal(t, "real", got.ProcessName)
	assert.Equal(t, 7, got.Pid)
	assert.Empty(t, got.Constraint, "stale section's constraint must not leak")
	assert.Empty(t, got.TopTasks, "stale section's tasks must not leak")
}

func TestOomScannerSequentialKills(t *testing.T) {
	// Two complete dumps back-to-back must emit two independent events.
	// Real systems do cascade OOM kills when memory pressure is severe.
	second := []string{
		`mutter invoked oom-killer: gfp_mask=0, order=0, oom_score_adj=0`,
		`Mem-Info:`,
		`oom-kill:constraint=CONSTRAINT_MEMCG,task=mutter,pid=99,uid=1000`,
		`Out of memory: Killed process 99 (mutter) total-vm:1kB, anon-rss:50kB, file-rss:0kB, shmem-rss:0kB, UID:1000 pgtables:0kB oom_score_adj:0`,
	}
	combined := append([]string{}, canonicalOomDump...)
	combined = append(combined, second...)

	var s oomScanner
	got := feedAll(&s, combined, time.Now())
	require.Len(t, got, 2)
	assert.Equal(t, "chromium", got[0].ProcessName)
	assert.Equal(t, "none", got[0].Constraint)
	assert.Equal(t, "mutter", got[1].ProcessName)
	assert.Equal(t, "memcg", got[1].Constraint)
}

func TestOomScannerNoiseWatchdogReleasesStuckSection(t *testing.T) {
	var s oomScanner
	s.feed("invoked oom-killer:", time.Now())
	for i := 0; i < oomScannerWatchdog+10; i++ {
		s.feed("filler line that matches no pattern", time.Now())
	}
	// Scanner should have reset; a closing line now lands as orphaned
	// preamble and is ignored.
	got := s.feed(`Out of memory: Killed process 1 (x) total-vm:0kB, anon-rss:0kB, file-rss:0kB, shmem-rss:0kB, UID:0 pgtables:0kB oom_score_adj:0`, time.Now())
	assert.Nil(t, got)
}

func TestOomScannerRecognisedLinesDoNotErodeWatchdog(t *testing.T) {
	// A Tasks state table with hundreds of entries should not trip the
	// watchdog — recognised lines are productive parsing, not noise.
	dump := []string{`chromium invoked oom-killer: gfp_mask=0, order=0, oom_score_adj=0`}
	for i := 0; i < oomScannerWatchdog+100; i++ {
		dump = append(dump,
			"[   "+itoa(i)+"]     0   "+itoa(i)+"     1234      567      45056        0             0 proc"+itoa(i))
	}
	dump = append(dump,
		`Out of memory: Killed process 1 (x) total-vm:0kB, anon-rss:0kB, file-rss:0kB, shmem-rss:0kB, UID:0 pgtables:0kB oom_score_adj:0`,
	)

	var s oomScanner
	got := feedAll(&s, dump, time.Now())
	require.Len(t, got, 1, "watchdog must not abandon a section composed only of recognised lines")
}

func TestConstraintFromKernel(t *testing.T) {
	cases := map[string]string{
		"NONE":             "none",
		"CPUSET":           "cpuset",
		"MEMCG":            "memcg",
		"MEMORY_POLICY":    "memory_policy",
		"SOMETHING_FUTURE": "something_future",
	}
	for raw, want := range cases {
		assert.Equal(t, want, constraintFromKernel(raw), raw)
	}
}

// itoa is a tiny test helper so the fixture builders don't pull in
// strconv noise. Bounded to non-negative ints.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
