package plan

import (
	"fmt"
	"regexp"
	"sort"
)

// ExecutionPlan represents a dependency-resolved ordering of jobs.
type ExecutionPlan struct {
	Jobs []*ExecutionJob
}

// BlockKind enumerates supported block types.
type BlockKind string

const (
	BlockKindJob      BlockKind = "job"
	BlockKindParallel BlockKind = "parallel"
	BlockKindSeries   BlockKind = "series"
)

// ExecutionBlock captures metadata about a block that produced one or more jobs.
type ExecutionBlock struct {
	ID                string
	Alias             string
	Name              string
	Path              string
	Kind              BlockKind
	Dir               string
	Env               map[string]string
	Queue             string
	Wait              string
	ContinueOnFailure bool
}

// ExecutionJob ties a plan job to its dependencies and block metadata.
type ExecutionJob struct {
	ID                string
	Alias             string
	Path              string
	Source            *Job
	Block             *ExecutionBlock
	Dependencies      []*JobDependency
	ContinueOnFailure bool
}

// JobDependency describes a resolved dependency on another job.
type JobDependency struct {
	Job      *ExecutionJob
	Optional bool
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)

// BuildExecutionPlan resolves IDs, validates dependencies, and returns the jobs in topological order.
func (f *File) BuildExecutionPlan() (*ExecutionPlan, error) {
	res := &resolver{
		file:      f,
		idTargets: make(map[string]*idTarget),
		aliasMap:  make(map[string]string),
	}
	if err := res.collect(); err != nil {
		return nil, err
	}
	if err := res.resolveDependencies(); err != nil {
		return nil, err
	}
	ordered, err := res.topologicalOrder()
	if err != nil {
		return nil, err
	}
	return &ExecutionPlan{Jobs: ordered}, nil
}

type resolver struct {
	file      *File
	blocks    []*blockContext
	jobs      []*jobContext
	idTargets map[string]*idTarget
	aliasMap  map[string]string

	blockCounter int
}

type blockContext struct {
	block      *ExecutionBlock
	rawDepends []depRef
	jobs       []*jobContext
}

type jobContext struct {
	job        *ExecutionJob
	rawDepends []depRef
}

type depRef struct {
	token    string
	job      *ExecutionJob
	optional bool
	path     string
}

type idTarget struct {
	block *blockContext
	job   *jobContext
}

func (t *idTarget) jobs() []*jobContext {
	if t == nil {
		return nil
	}
	if t.job != nil {
		return []*jobContext{t.job}
	}
	if t.block != nil {
		return t.block.jobs
	}
	return nil
}

func (r *resolver) collect() error {
	for idx, entry := range r.file.Jobs {
		path := fmt.Sprintf("jobs[%d]", idx)
		switch {
		case entry.Job != nil:
			if err := r.handleStandaloneJob(entry.Job, path); err != nil {
				return err
			}
		case entry.Parallel != nil:
			if err := r.handleParallel(entry.Parallel, path+".parallel"); err != nil {
				return err
			}
		case entry.Series != nil:
			if err := r.handleSeries(entry.Series, path+".series"); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s: must contain job, parallel, or series", path)
		}
	}
	return nil
}

func (r *resolver) handleStandaloneJob(job *Job, path string) error {
	blockID := r.nextBlockID()
	ctx, err := r.newJobContext(job, path, nil, blockID, 0)
	if err != nil {
		return err
	}
	// Allow referencing the single job via its ID; no block-level alias for single jobs.
	r.jobs = append(r.jobs, ctx)
	return nil
}

func (r *resolver) handleParallel(block *Parallel, path string) error {
	execBlock, err := r.newExecutionBlock(block.ID, block.Alias, block.Name, path, BlockKindParallel, block.Dir, block.Env, "", "", block.ContinueOnFailure)
	if err != nil {
		return err
	}
	ctx := &blockContext{block: execBlock}
	r.idTargets[execBlock.ID] = &idTarget{block: ctx}
	ctx.rawDepends = r.buildDepRefs(block.DependsOn, block.ContinueOnFailure, path+".depends_on")

	for idx := range block.Jobs {
		job := &block.Jobs[idx]
		jobCtx, err := r.newJobContext(job, fmt.Sprintf("%s.jobs[%d]", path, idx), ctx, execBlock.ID, idx)
		if err != nil {
			return err
		}
		ctx.jobs = append(ctx.jobs, jobCtx)
		r.jobs = append(r.jobs, jobCtx)
	}
	r.blocks = append(r.blocks, ctx)
	return nil
}

func (r *resolver) handleSeries(block *Series, path string) error {
	waitMode := block.Wait
	if waitMode == "" {
		waitMode = "success"
	}
	execBlock, err := r.newExecutionBlock(block.ID, block.Alias, block.Name, path, BlockKindSeries, block.Dir, block.Env, block.Queue, waitMode, block.ContinueOnFailure)
	if err != nil {
		return err
	}
	ctx := &blockContext{block: execBlock}
	r.idTargets[execBlock.ID] = &idTarget{block: ctx}
	ctx.rawDepends = r.buildDepRefs(block.DependsOn, block.ContinueOnFailure, path+".depends_on")

	for idx := range block.Jobs {
		job := &block.Jobs[idx]
		jobCtx, err := r.newJobContext(job, fmt.Sprintf("%s.jobs[%d]", path, idx), ctx, execBlock.ID, idx)
		if err != nil {
			return err
		}
		if idx > 0 {
			prev := ctx.jobs[idx-1].job
			jobCtx.rawDepends = append(jobCtx.rawDepends, depRef{
				job:      prev,
				optional: waitMode == "any",
				path:     fmt.Sprintf("%s.jobs[%d]", path, idx),
			})
		}
		ctx.jobs = append(ctx.jobs, jobCtx)
		r.jobs = append(r.jobs, jobCtx)
	}
	r.blocks = append(r.blocks, ctx)
	return nil
}

func (r *resolver) nextBlockID() string {
	id := fmt.Sprintf("block%d", r.blockCounter)
	r.blockCounter++
	return id
}

func (r *resolver) newExecutionBlock(id, alias, name, path string, kind BlockKind, dir string, env map[string]string, queue string, wait string, cof bool) (*ExecutionBlock, error) {
	blockID := id
	if blockID == "" {
		blockID = r.nextBlockID()
	}
	if err := r.registerID(blockID, path+".id", "block"); err != nil {
		return nil, err
	}
	if alias != "" {
		if err := r.registerAlias(alias, blockID, path+".alias"); err != nil {
			return nil, err
		}
	}
	exec := &ExecutionBlock{
		ID:                blockID,
		Alias:             alias,
		Name:              name,
		Path:              path,
		Kind:              kind,
		Dir:               dir,
		Env:               copyEnv(env),
		Queue:             queue,
		Wait:              wait,
		ContinueOnFailure: cof,
	}
	return exec, nil
}

func (r *resolver) newJobContext(job *Job, path string, blockCtx *blockContext, baseID string, idx int) (*jobContext, error) {
	if job.Command == "" {
		return nil, fmt.Errorf("%s: missing command", path)
	}
	if job.ID == "" {
		suffix := fmt.Sprintf("job%d", idx)
		if baseID == "" {
			baseID = r.nextBlockID()
		}
		job.ID = fmt.Sprintf("%s.%s", baseID, suffix)
	}
	if err := r.registerID(job.ID, path+".id", "job"); err != nil {
		return nil, err
	}
	if job.Alias != "" {
		if err := r.registerAlias(job.Alias, job.ID, path+".alias"); err != nil {
			return nil, err
		}
	}

	execJob := &ExecutionJob{
		ID:                job.ID,
		Alias:             job.Alias,
		Path:              path,
		Source:            job,
		Block:             nil,
		ContinueOnFailure: job.ContinueOnFailure,
	}
	if blockCtx != nil {
		execJob.Block = blockCtx.block
	}
	ctx := &jobContext{job: execJob}
	ctx.rawDepends = append(ctx.rawDepends, r.buildDepRefs(job.DependsOn, false, path+".depends_on")...)
	if blockCtx != nil {
		for _, blockDep := range blockCtx.rawDepends {
			ctx.rawDepends = append(ctx.rawDepends, blockDep)
		}
	}
	r.idTargets[job.ID] = &idTarget{job: ctx}
	return ctx, nil
}

func (r *resolver) buildDepRefs(tokens []string, optional bool, basePath string) []depRef {
	if len(tokens) == 0 {
		return nil
	}
	refs := make([]depRef, 0, len(tokens))
	for i, token := range tokens {
		refs = append(refs, depRef{
			token:    token,
			optional: optional,
			path:     fmt.Sprintf("%s[%d]", basePath, i),
		})
	}
	return refs
}

func (r *resolver) registerID(id, path, kind string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("%s: invalid identifier %q (allowed: letters, numbers, '_', '-', '.', ':')", path, id)
	}
	if existing, ok := r.idTargets[id]; ok {
		if existing.block != nil && kind == "block" {
			return fmt.Errorf("%s: duplicate block id %q", path, id)
		}
		if existing.job != nil && kind == "job" {
			return fmt.Errorf("%s: duplicate job id %q", path, id)
		}
		return fmt.Errorf("%s: identifier %q already in use", path, id)
	}
	r.idTargets[id] = &idTarget{}
	return nil
}

func (r *resolver) registerAlias(alias, targetID, path string) error {
	if !idPattern.MatchString(alias) {
		return fmt.Errorf("%s: invalid alias %q (allowed: letters, numbers, '_', '-', '.', ':')", path, alias)
	}
	if _, ok := r.idTargets[alias]; ok {
		if targetID != alias {
			return fmt.Errorf("%s: alias %q conflicts with an existing ID", path, alias)
		}
	}
	if existing, ok := r.aliasMap[alias]; ok {
		if existing != targetID {
			return fmt.Errorf("%s: alias %q already used", path, alias)
		}
		return nil
	}
	r.aliasMap[alias] = targetID
	return nil
}

func (r *resolver) resolveToken(token, path string) (*idTarget, error) {
	if token == "" {
		return nil, fmt.Errorf("%s: dependency cannot be empty", path)
	}
	canonical := token
	if mapped, ok := r.aliasMap[token]; ok {
		canonical = mapped
	}
	target, ok := r.idTargets[canonical]
	if !ok {
		return nil, fmt.Errorf("%s: unknown dependency %q", path, token)
	}
	return target, nil
}

func (r *resolver) resolveDependencies() error {
	for _, jc := range r.jobs {
		jc.job.Dependencies = nil
		seen := make(map[string]bool)
		for _, ref := range jc.rawDepends {
			optional := ref.optional || jc.job.ContinueOnFailure
			if ref.job != nil {
				if err := r.addDependency(jc.job, ref.job, optional, seen, ref.path); err != nil {
					return err
				}
				continue
			}
			target, err := r.resolveToken(ref.token, ref.path)
			if err != nil {
				return err
			}
			jobTargets := target.jobs()
			if len(jobTargets) == 0 {
				return fmt.Errorf("%s: dependency %q has no jobs to wait for", ref.path, ref.token)
			}
			for _, depJob := range jobTargets {
				if err := r.addDependency(jc.job, depJob.job, optional, seen, ref.path); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (r *resolver) addDependency(job *ExecutionJob, dep *ExecutionJob, optional bool, seen map[string]bool, path string) error {
	if job.ID == dep.ID {
		return fmt.Errorf("%s: job %q cannot depend on itself", path, job.ID)
	}
	if job.Source.Host != "" && dep.Source.Host != "" && job.Source.Host != dep.Source.Host {
		return fmt.Errorf("%s: cross-host dependency from %s (%s) to %s (%s) is not supported",
			path, job.ID, job.Source.Host, dep.ID, dep.Source.Host)
	}
	if seen[dep.ID] {
		return nil
	}
	seen[dep.ID] = true
	job.Dependencies = append(job.Dependencies, &JobDependency{
		Job:      dep,
		Optional: optional,
	})
	return nil
}

func (r *resolver) topologicalOrder() ([]*ExecutionJob, error) {
	indegree := make(map[*ExecutionJob]int)
	graph := make(map[*ExecutionJob][]*ExecutionJob)

	for _, jc := range r.jobs {
		job := jc.job
		if _, ok := indegree[job]; !ok {
			indegree[job] = 0
		}
		for _, dep := range job.Dependencies {
			graph[dep.Job] = append(graph[dep.Job], job)
			indegree[job]++
		}
	}

	var ready []*ExecutionJob
	for job, degree := range indegree {
		if degree == 0 {
			ready = append(ready, job)
		}
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i].Path < ready[j].Path })

	var ordered []*ExecutionJob
	for len(ready) > 0 {
		current := ready[0]
		ready = ready[1:]
		ordered = append(ordered, current)
		for _, dependent := range graph[current] {
			indegree[dependent]--
			if indegree[dependent] == 0 {
				ready = append(ready, dependent)
				sort.Slice(ready, func(i, j int) bool { return ready[i].Path < ready[j].Path })
			}
		}
	}

	if len(ordered) != len(indegree) {
		return nil, fmt.Errorf("circular dependency detected in plan")
	}

	return ordered, nil
}

func copyEnv(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}
