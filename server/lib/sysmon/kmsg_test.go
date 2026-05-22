package sysmon

import (
	"testing"
)

func TestParseKmsgLine(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			"standard envelope",
			"<4,123,456789,->;Out of memory: Killed process 1 (init) total-vm:1kB",
			"Out of memory: Killed process 1 (init) total-vm:1kB",
		},
		{
			"no envelope",
			"naked message",
			"naked message",
		},
		{
			"empty after semicolon",
			"<3,1,2,->;",
			"",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseKmsgLine(tc.in); got != tc.want {
				t.Fatalf("parseKmsgLine = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseOomKill(t *testing.T) {
	t.Run("canonical line", func(t *testing.T) {
		msg := "Out of memory: Killed process 1234 (chromium) total-vm:5234572kB, anon-rss:4823900kB, file-rss:100kB, shmem-rss:200kB, UID:0 pgtables:8000kB oom_score_adj:0"
		data := parseOomKill(msg)
		if data == nil {
			t.Fatal("expected match")
		}
		if data.Pid != 1234 {
			t.Errorf("Pid = %d, want 1234", data.Pid)
		}
		if data.ProcessName != "chromium" {
			t.Errorf("ProcessName = %q, want chromium", data.ProcessName)
		}
		if data.TotalVmKb == nil || *data.TotalVmKb != 5234572 {
			t.Errorf("TotalVmKb = %v, want 5234572", data.TotalVmKb)
		}
		// rss_kb = anon + file + shmem = 4823900 + 100 + 200
		if data.RssKb != 4824200 {
			t.Errorf("RssKb = %d, want 4824200", data.RssKb)
		}
		if data.OomScoreAdj == nil || *data.OomScoreAdj != 0 {
			t.Errorf("OomScoreAdj = %v, want 0", data.OomScoreAdj)
		}
	})

	t.Run("negative oom_score_adj", func(t *testing.T) {
		msg := "Out of memory: Killed process 999 (sshd) total-vm:10kB, anon-rss:1kB, file-rss:2kB, shmem-rss:0kB, UID:0 pgtables:1kB oom_score_adj:-1000"
		data := parseOomKill(msg)
		if data == nil {
			t.Fatal("expected match")
		}
		if data.OomScoreAdj == nil || *data.OomScoreAdj != -1000 {
			t.Errorf("OomScoreAdj = %v, want -1000", data.OomScoreAdj)
		}
	})

	t.Run("comm with internal space", func(t *testing.T) {
		// Kernel preserves spaces in comm; bounded by TASK_COMM_LEN.
		msg := "Out of memory: Killed process 42 (kworker u4:1) total-vm:0kB, anon-rss:0kB, file-rss:0kB, shmem-rss:0kB, UID:0 pgtables:0kB oom_score_adj:0"
		data := parseOomKill(msg)
		if data == nil {
			t.Fatal("expected match")
		}
		if data.ProcessName != "kworker u4:1" {
			t.Errorf("ProcessName = %q, want %q", data.ProcessName, "kworker u4:1")
		}
	})

	t.Run("no match", func(t *testing.T) {
		if got := parseOomKill("just some other kernel log line"); got != nil {
			t.Fatalf("expected nil, got %+v", got)
		}
	})

	t.Run("preamble (oom dump task list)", func(t *testing.T) {
		// We must NOT match the per-process audit lines the kernel emits
		// before the canonical "Killed process" decision.
		msg := "[1234]   0  1234   1308611  1205975  9678848        0             0 chromium"
		if got := parseOomKill(msg); got != nil {
			t.Fatalf("expected nil for preamble line, got %+v", got)
		}
	})
}
