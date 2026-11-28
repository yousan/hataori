package worker

import "github.com/yousan/hataori/internal/workerproto"

func (r *Resident) checkAdmission(target workerproto.Target, a *workerproto.Admission) *workerproto.Refusal {
	if a == nil {
		return nil
	}
	t := r.options.Telemetry()
	if !workerproto.FreshTelemetry(t, r.options.Now().UnixMilli()) || t.MemoryFree == 0 || t.DiskFree == 0 {
		return failure("unavailable", "resource observation is unknown or stale")
	}
	resident, starting := 0, 0
	for _, run := range r.state.Runs {
		if run.Target == target || (run.State == "ended" || run.State == "gone") {
			continue
		}
		resident++
		if run.State == "prepared" || run.State == "starting" || run.State == "unknown" {
			starting++
		}
	}
	if a.MaxResident > 0 && resident >= a.MaxResident || a.MaxStarting > 0 && starting >= a.MaxStarting {
		return failure("unavailable", "resident or starting slots are full")
	}
	if a.MaxLoad > 0 && t.Load >= a.MaxLoad || t.MemoryFree < a.MinFreeMemory || t.DiskFree < a.MinFreeDisk {
		return failure("unavailable", "configured admission resource limit reached")
	}
	return nil
}
