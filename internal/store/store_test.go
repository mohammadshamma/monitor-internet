package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestCloseOutageIncludesFullCycleInterval(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	// Three consecutive failing cycles at a 5s interval: t=0s, t=5s, t=10s.
	// The outage starts at the first bad cycle (0s) and its last bad cycle is
	// at 10s, covering 3 * 5s = 15s total.
	start := time.Unix(1_700_000_000, 0)
	end := start.Add(2 * 5 * time.Second)

	id, err := st.OpenOutage(start, "ISP_FAULT", "gateway=192.168.1.1 isp_edge=203.0.113.1")
	if err != nil {
		t.Fatalf("OpenOutage: %v", err)
	}
	if err := st.CloseOutage(id, end, "ISP_FAULT"); err != nil {
		t.Fatalf("CloseOutage: %v", err)
	}

	var gotDuration int64
	if err := st.DB().QueryRow(`SELECT duration_s FROM outages WHERE id = ?`, id).Scan(&gotDuration); err != nil {
		t.Fatalf("query duration_s: %v", err)
	}
	if gotDuration != 15 {
		t.Errorf("duration_s = %d, want 15 (3 failing 5s cycles)", gotDuration)
	}
}
