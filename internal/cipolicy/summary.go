package cipolicy

import (
	"context"
	"fmt"
	"sort"
	"time"
)

type JobTiming struct {
	Name, Status, Conclusion          string
	Labels                            []string
	StartDelaySeconds, ElapsedSeconds *int64
}
type RunSummary struct {
	Run                                                                    int64
	Head, Event, Status, Conclusion                                        string
	Attempt, AdditionalAttempts                                            int
	Jobs                                                                   []JobTiming
	Evidence                                                               string
	Tokens, Money, ReviewTime, EscapedRegressions, RepairCost, RunnerQueue string
}

func seconds(from, to time.Time) *int64 {
	if from.IsZero() || to.IsZero() || to.Before(from) {
		return nil
	}
	n := int64(to.Sub(from) / time.Second)
	return &n
}
func (a *Adapter) Summarize(ctx context.Context, id int64) (RunSummary, error) {
	ctx = a.budget(ctx)
	s := RunSummary{Run: id, Evidence: "metadata only; no validation or release claim", Tokens: "unknown", Money: "unknown", ReviewTime: "unknown", EscapedRegressions: "unknown", RepairCost: "unknown", RunnerQueue: "unknown; start delay includes dependencies"}
	if id <= 0 {
		return s, fmt.Errorf("run identity required")
	}
	if err := a.repository(ctx); err != nil {
		return s, err
	}
	var r runRecord
	if err := a.request(ctx, "GET", fmt.Sprintf("%s/actions/runs/%d", a.prefix(), id), nil, &r); err != nil {
		return s, err
	}
	if r.ID != id || r.Repository.ID != a.root.RepositoryID || r.Repository.FullName != a.root.Repository || r.RunAttempt < 1 {
		return s, fmt.Errorf("summary repository/run mismatch")
	}
	s.Head = r.HeadSHA
	s.Event = r.Event
	s.Status = r.Status
	s.Conclusion = r.Conclusion
	s.Attempt = r.RunAttempt
	s.AdditionalAttempts = r.RunAttempt - 1
	jobs, err := a.jobs(ctx, Sweep{Target: r.HeadSHA}, id, r.RunAttempt)
	if err != nil {
		return s, err
	}
	for _, j := range jobs {
		if j.Conclusion == "skipped" {
			j.StartedAt = time.Time{}
			j.CompletedAt = time.Time{}
		}
		s.Jobs = append(s.Jobs, JobTiming{j.Name, j.Status, j.Conclusion, j.Labels, seconds(r.CreatedAt, j.StartedAt), seconds(j.StartedAt, j.CompletedAt)})
	}
	sort.Slice(s.Jobs, func(i, j int) bool { return s.Jobs[i].Name < s.Jobs[j].Name })
	return s, nil
}
