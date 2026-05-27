package sysmon

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// pageSizeKB is the kernel page size in KiB used to convert page-denominated
// fields from the OOM dump (RSS in `Tasks state`, free/total in Mem-Info)
// into KiB. The kernel-images target is x86_64 with the standard 4 KiB page;
// this constant is wrong on architectures with a different page size.
const pageSizeKB = 4

// topTasksN bounds the number of process entries from the kernel's Tasks
// state table that we surface in the OOM event payload. Five is enough to
// answer "what consumed memory" for a typical Chromium-on-VM workload
// without bloating the event.
const topTasksN = 5

// oomScannerWatchdog bounds the number of UNRECOGNIZED kmsg messages we
// will tolerate inside a single OOM section before abandoning it.
// Recognised lines (Mem-Info, Tasks state, constraint, killed) don't
// count toward the budget, so the watchdog only trips when the section
// diverges from the expected kernel format. A busy VM can emit several
// hundred Tasks state rows during a single dump; this budget leaves
// headroom for that plus the full Mem-Info block.
const oomScannerWatchdog = 2000

// OomInstance is a parsed kernel OOM-killer event extracted from /dev/kmsg.
//
// Fields map to BrowserSystemOomKillEventData. Optional fields use the
// zero value when the kernel did not emit the corresponding kmsg line;
// the publisher decides whether to encode them.
type OomInstance struct {
	// ProcessName is the comm of the killed process, bounded to 15 chars
	// by the kernel (TASK_COMM_LEN-1). May contain spaces.
	ProcessName string
	// Pid is the PID of the killed process.
	Pid int
	// RssKb is the sum of anon-rss, file-rss, and shmem-rss in KiB.
	// Zero if the kernel format did not include the per-class breakdown.
	RssKb int
	// Constraint is one of "none", "memcg", "cpuset", "memory_policy",
	// extracted from the structured `oom-kill:` line that kernels >= 5.0
	// emit. Empty on older kernels.
	Constraint string
	// MemTotalKb is the total system memory at the time of the kill,
	// derived from the `N pages RAM` line. Zero if not parseable.
	MemTotalKb int
	// MemFreeKb is free memory at the time of the kill, derived from the
	// `free:N` field in Mem-Info. Zero if not parseable.
	MemFreeKb int
	// TopTasks is up to topTasksN processes from the Tasks state table,
	// sorted by RSS descending. Nil if the kernel did not emit the table.
	TopTasks []TaskMemSnapshot
	// TimeOfDeath is the timestamp of the closing "Killed process" line
	// as reported by the kmsg envelope.
	TimeOfDeath time.Time
}

// TaskMemSnapshot is one row from the kernel's Tasks state dump,
// representing a single process's memory footprint at the moment of the
// OOM kill.
type TaskMemSnapshot struct {
	Pid   int
	Name  string
	RssKb int
}

// KmsgMessage is the minimal subset of a /dev/kmsg record that the OOM
// state machine consumes. Decoupling from the underlying kmsg library
// lets the parser run portably under unit tests; the production wiring
// lives in kmsg_linux.go.
type KmsgMessage struct {
	Timestamp time.Time
	Body      string
}

var (
	oomStartRe = regexp.MustCompile(`invoked oom-killer:`)
	// Modern (Linux >= 5.0) structured constraint+task line.
	// Example:
	//   oom-kill:constraint=CONSTRAINT_NONE,nodemask=(null),cpuset=/,mems_allowed=0,global_oom,task_memcg=/,task=chromium,pid=1234,uid=0
	oomConstraintRe = regexp.MustCompile(`oom-kill:constraint=CONSTRAINT_(\w+)`)
	// Closing line. Example:
	//   Out of memory: Killed process 1234 (chromium) total-vm:5234572kB, anon-rss:4823900kB, file-rss:100kB, shmem-rss:200kB, UID:0 pgtables:8000kB oom_score_adj:0
	oomKilledRe = regexp.MustCompile(`Killed process (\d+) \(([^)]+)\)`)
	// RSS breakdown is matched independently so the killed-line match
	// remains useful on kernels that omit per-class RSS fields.
	oomAnonRssRe  = regexp.MustCompile(`anon-rss:(\d+)kB`)
	oomFileRssRe  = regexp.MustCompile(`file-rss:(\d+)kB`)
	oomShmemRssRe = regexp.MustCompile(`shmem-rss:(\d+)kB`)
	// Mem-Info / total RAM. Example: `524288 pages RAM`.
	oomTotalPagesRe = regexp.MustCompile(`(\d+)\s+pages\s+RAM`)
	// Mem-Info free-pages triple. Example: ` free:4560 free_pcp:0 free_cma:0`.
	// The trailing `free_pcp:` anchor distinguishes this from per-zone
	// lines like `Node 0 DMA free:11264kB boost:0kB`, which carry kB
	// units rather than raw page counts.
	oomFreePagesRe = regexp.MustCompile(`(?:^|\s)free:(\d+)\s+free_pcp:`)
	// Tasks state row. Columns (post-bracket): uid tgid total_vm rss
	// pgtables_bytes swapents oom_score_adj name. RSS is in pages.
	// Example:
	//   [   1234]   1000  1234  1308611  1205975    9678848        0             0 chromium
	oomTaskEntryRe = regexp.MustCompile(`^\[\s*(\d+)\]\s+\d+\s+\d+\s+\d+\s+(\d+)\s+\d+\s+\d+\s+-?\d+\s+(\S.*?)\s*$`)
)

// oomScanner is a state machine that turns a stream of kmsg message
// bodies into completed OomInstance values. It tolerates the kernel
// emitting many intermediate lines (stack traces, Mem-Info zone
// breakdowns, the Tasks state table) between the opening "invoked
// oom-killer" line and the closing "Killed process" line, and recovers
// if a section never closes.
type oomScanner struct {
	pending  *OomInstance
	noiseBuf int
}

// feed consumes a single kmsg message body and returns a completed
// OomInstance when the scanner observes the closing "Killed process"
// line. All other inputs return nil; the scanner accumulates partial
// state internally.
func (s *oomScanner) feed(body string, ts time.Time) *OomInstance {
	if oomStartRe.MatchString(body) {
		// New section. If a previous section is still pending, the
		// kernel either failed to emit the closing line or the kmsg
		// ring buffer dropped it; abandon and start fresh.
		s.pending = &OomInstance{TimeOfDeath: ts}
		s.noiseBuf = 0
		return nil
	}

	if s.pending == nil {
		return nil
	}

	if m := oomKilledRe.FindStringSubmatch(body); m != nil {
		pid, _ := strconv.Atoi(m[1])
		s.pending.Pid = pid
		s.pending.ProcessName = m[2]
		s.pending.RssKb = sumRss(body)
		s.pending.TimeOfDeath = ts
		trimTopTasks(s.pending)
		out := s.pending
		s.pending = nil
		s.noiseBuf = 0
		return out
	}

	// Each recognised line resets the noise watchdog; only unparseable
	// intermediate lines erode the budget.
	matched := false
	if m := oomConstraintRe.FindStringSubmatch(body); m != nil {
		s.pending.Constraint = constraintFromKernel(m[1])
		matched = true
	}
	if m := oomTotalPagesRe.FindStringSubmatch(body); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			s.pending.MemTotalKb = n * pageSizeKB
		}
		matched = true
	}
	if m := oomFreePagesRe.FindStringSubmatch(body); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			s.pending.MemFreeKb = n * pageSizeKB
		}
		matched = true
	}
	if m := oomTaskEntryRe.FindStringSubmatch(body); m != nil {
		pid, _ := strconv.Atoi(m[1])
		rss, _ := strconv.Atoi(m[2])
		s.pending.TopTasks = append(s.pending.TopTasks, TaskMemSnapshot{
			Pid:   pid,
			Name:  m[3],
			RssKb: rss * pageSizeKB,
		})
		matched = true
	}

	if !matched {
		s.noiseBuf++
		if s.noiseBuf > oomScannerWatchdog {
			s.pending = nil
			s.noiseBuf = 0
		}
	}
	return nil
}

// trimTopTasks sorts the accumulated Tasks state entries by RSS
// descending and caps the slice at topTasksN.
func trimTopTasks(o *OomInstance) {
	if len(o.TopTasks) == 0 {
		return
	}
	sort.Slice(o.TopTasks, func(i, j int) bool {
		return o.TopTasks[i].RssKb > o.TopTasks[j].RssKb
	})
	if len(o.TopTasks) > topTasksN {
		o.TopTasks = o.TopTasks[:topTasksN]
	}
}

// constraintFromKernel converts the kernel's CONSTRAINT_* identifier
// into the lowercase form used in the public event schema. Unknown
// values are passed through lowercased so the kernel's exact label still
// reaches logs even if it falls outside the schema enum.
func constraintFromKernel(raw string) string {
	switch raw {
	case "NONE":
		return "none"
	case "CPUSET":
		return "cpuset"
	case "MEMCG":
		return "memcg"
	case "MEMORY_POLICY":
		return "memory_policy"
	default:
		return strings.ToLower(raw)
	}
}

// sumRss extracts anon-rss, file-rss, and shmem-rss from the canonical
// "Killed process" line and returns their sum in KiB. Missing fields
// contribute zero, so older kernels that omit the breakdown yield 0.
func sumRss(body string) int {
	var total int
	for _, re := range []*regexp.Regexp{oomAnonRssRe, oomFileRssRe, oomShmemRssRe} {
		if m := re.FindStringSubmatch(body); m != nil {
			n, _ := strconv.Atoi(m[1])
			total += n
		}
	}
	return total
}
