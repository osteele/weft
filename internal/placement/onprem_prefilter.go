package placement

import (
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/predictor"
)

var loadInventoryHosts = inventory.LoadHosts
var collectOnPremMetrics = CollectMetrics
var scoreOnPremHosts = ScoreHostListWithPredictor
var buildJobPredictorFromConfig = BuildJobPredictorFromConfig
var resolvePredictBatchForOnPrem = predictor.ResolvePredictBatch

type PrefilterCallbacks struct {
	OnProgress func(current, total int)
	OnPhase    func(phase string)
	OnPlaced   func(jobID int64, host string)
}

func PrefilterOnPrem(database *sql.DB, jobs []*db.Job, cfg *config.Config, callbacks PrefilterCallbacks) []*db.Job {
	hosts, err := loadInventoryHosts()
	if err != nil || len(hosts) == 0 {
		return prefilterOnPremIndividually(database, jobs, cfg, callbacks)
	}

	hostNames := make([]string, 0, len(hosts))
	for _, host := range hosts {
		hostNames = append(hostNames, host.Name)
	}
	if callbacks.OnPhase != nil {
		callbacks.OnPhase(fmt.Sprintf("Collecting on-prem metrics for %d host(s)...", len(hosts)))
	}
	liveMetrics := collectOnPremMetrics(database, hostNames, 5*time.Second)
	predictors := BuildBatchPredictors(cfg, jobs, hosts, callbacks.OnPhase)

	var remaining []*db.Job
	total := len(jobs)
	for i, job := range jobs {
		if callbacks.OnProgress != nil {
			callbacks.OnProgress(i+1, total)
		}
		host, ok := SelectHostFromSnapshot(database, hosts, liveMetrics, job, cfg, predictors[job.ID])
		if !ok {
			remaining = append(remaining, job)
			continue
		}
		assigned, err := db.AssignJobHost(database, job.ID, host)
		if err != nil || !assigned {
			remaining = append(remaining, job)
			continue
		}
		slog.Info("placed job on-prem during launch pre-filter", "job_id", job.ID, "host", host)
		if callbacks.OnPlaced != nil {
			callbacks.OnPlaced(job.ID, host)
		}
	}
	return remaining
}

func prefilterOnPremIndividually(database *sql.DB, jobs []*db.Job, cfg *config.Config, callbacks PrefilterCallbacks) []*db.Job {
	var remaining []*db.Job
	total := len(jobs)
	for i, job := range jobs {
		if callbacks.OnProgress != nil {
			callbacks.OnProgress(i+1, total)
		}
		constraints := ConstraintsFromJob(job)
		predict := BuildJobPredictorFromConfig(cfg, constraints)
		plan, err := Evaluate(EvaluateRequest{
			Constraints: constraints,
			Predictor:   predict,
			Sources:     []CandidateSource{&OnPremSource{}},
			Database:    database,
		})
		if err != nil || plan.Unplaced || plan.Cheap == nil || plan.Cheap.OnPrem == nil {
			remaining = append(remaining, job)
			continue
		}
		host := plan.Cheap.OnPrem.Host
		assigned, err := db.AssignJobHost(database, job.ID, host)
		if err != nil || !assigned {
			remaining = append(remaining, job)
			continue
		}
		slog.Info("placed job on-prem during launch pre-filter", "job_id", job.ID, "host", host)
		if callbacks.OnPlaced != nil {
			callbacks.OnPlaced(job.ID, host)
		}
	}
	return remaining
}

func SelectHostFromSnapshot(database *sql.DB, hosts []inventory.HostSpec, liveMetrics map[string]*HostMetrics, job *db.Job, cfg *config.Config, predict JobPredictor) (string, bool) {
	if job == nil {
		return "", false
	}
	constraints := ConstraintsFromJob(job)
	if db.HasRentalTag(constraints.Tags) {
		return "", false
	}

	if predict == nil {
		predict = buildJobPredictorFromConfig(cfg, constraints)
	}
	reachableHosts := FilterReachableHosts(hosts, liveMetrics)
	if host, ok := FirstEligibleHost(database, reachableHosts, constraints, liveMetrics, predict); ok {
		return host, true
	}
	return FirstEligibleHost(database, hosts, constraints, nil, predict)
}

func BuildBatchPredictors(cfg *config.Config, jobs []*db.Job, hosts []inventory.HostSpec, onPhase func(string)) map[int64]JobPredictor {
	predCfg := PredictorConfigFromApp(cfg)
	if !predCfg.Configured() || len(jobs) == 0 || len(hosts) == 0 {
		return nil
	}

	type batchRef struct {
		jobID int64
		host  string
	}

	refs := make(map[int64]batchRef)
	batchJobs := make([]predictor.BatchJob, 0, len(jobs)*len(hosts))
	var nextID int64 = 1
	for _, job := range jobs {
		if job == nil {
			continue
		}
		constraints := ConstraintsFromJob(job)
		if constraints.Command == "" || db.HasRentalTag(constraints.Tags) {
			continue
		}
		for _, host := range hosts {
			batchJobs = append(batchJobs, predictor.BatchJob{
				ID:         nextID,
				Command:    constraints.Command,
				Host:       host.Name,
				Project:    constraints.Project,
				GPUClass:   constraints.GPUClass,
				WorkingDir: job.WorkingDir,
			})
			refs[nextID] = batchRef{jobID: job.ID, host: host.Name}
			nextID++
		}
	}
	if len(batchJobs) == 0 {
		return nil
	}

	if onPhase != nil {
		onPhase(fmt.Sprintf("Estimating on-prem runtimes for %d host/job pair(s)...", len(batchJobs)))
	}

	results, err := resolvePredictBatchForOnPrem(predCfg, batchJobs)
	if err != nil || results == nil {
		return nil
	}

	predictionsByJob := make(map[int64]map[string]*RawPrediction)
	for id, ref := range refs {
		result := results[id]
		if result == nil {
			continue
		}
		raw := RawPredictionFromResult(result)
		if raw == nil {
			continue
		}
		perHost := predictionsByJob[ref.jobID]
		if perHost == nil {
			perHost = make(map[string]*RawPrediction)
			predictionsByJob[ref.jobID] = perHost
		}
		perHost[ref.host] = raw
	}
	if len(predictionsByJob) == 0 {
		return nil
	}

	predictors := make(map[int64]JobPredictor, len(predictionsByJob))
	for jobID, perHost := range predictionsByJob {
		hostPredictions := perHost
		predictors[jobID] = NewJobPredictor(func(host string) *RawPrediction {
			return hostPredictions[host]
		})
	}
	return predictors
}

func RawPredictionFromResult(result *predictor.Result) *RawPrediction {
	if result == nil {
		return nil
	}
	raw := &RawPrediction{}
	if result.DurationS != nil {
		raw.DurationS = &RawPredictionField{
			Mean:  result.DurationS.Mean,
			Lower: result.DurationS.Lower,
			Upper: result.DurationS.Upper,
		}
	}
	if result.PeakRSSKB != nil {
		raw.PeakRSSKB = &RawPredictionField{
			Mean:  result.PeakRSSKB.Mean,
			Lower: result.PeakRSSKB.Lower,
			Upper: result.PeakRSSKB.Upper,
		}
	}
	if result.MaxGPUMemMiB != nil {
		raw.MaxGPUMemMiB = &RawPredictionField{
			Mean:  result.MaxGPUMemMiB.Mean,
			Lower: result.MaxGPUMemMiB.Lower,
			Upper: result.MaxGPUMemMiB.Upper,
		}
	}
	if raw.DurationS == nil && raw.PeakRSSKB == nil && raw.MaxGPUMemMiB == nil {
		return nil
	}
	return raw
}

func FilterReachableHosts(hosts []inventory.HostSpec, liveMetrics map[string]*HostMetrics) []inventory.HostSpec {
	if len(liveMetrics) == 0 {
		return nil
	}
	reachable := make([]inventory.HostSpec, 0, len(hosts))
	for _, host := range hosts {
		if liveMetrics[host.Name] != nil {
			reachable = append(reachable, host)
		}
	}
	return reachable
}

func FirstEligibleHost(database *sql.DB, hosts []inventory.HostSpec, constraints Constraints, metrics map[string]*HostMetrics, predict JobPredictor) (string, bool) {
	if len(hosts) == 0 {
		return "", false
	}
	scores := scoreOnPremHosts(database, hosts, constraints, metrics, predict)
	for _, score := range scores {
		if score.Eligible {
			return score.Host, true
		}
	}
	return "", false
}
