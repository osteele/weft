package main

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"sync"
	"time"

	"github.com/osteele/weft/internal/cloudlog"
	"github.com/osteele/weft/internal/oplog"
)

const liveLogUploadInterval = 60 * time.Second

type liveLogUploadState struct {
	partHashes   map[int]uint64
	manifestJSON string
}

func startLogUploader(bucket string, jobID, runID int64, logPath string) func() {
	var once sync.Once
	done := make(chan struct{})
	stopped := make(chan struct{})
	state := &liveLogUploadState{partHashes: make(map[int]uint64)}

	stop := func() {
		once.Do(func() { close(done) })
		<-stopped
	}

	go func() {
		defer close(stopped)
		ticker := time.NewTicker(liveLogUploadInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				uploadLiveLog(bucket, jobID, runID, logPath, state)
				return
			case <-ticker.C:
				uploadLiveLog(bucket, jobID, runID, logPath, state)
			}
		}
	}()

	return stop
}

func uploadLiveLog(bucket string, jobID, runID int64, logPath string, state *liveLogUploadState) {
	data, err := os.ReadFile(logPath)
	if err != nil || len(data) == 0 {
		return
	}

	manifest, chunks := cloudlog.BuildRunChunks(jobID, runID, data, cloudlog.DefaultChunkTargetBytes)
	if len(chunks) == 0 {
		return
	}

	for _, chunk := range chunks {
		hash := hashChunk(chunk.Content)
		if state.partHashes[chunk.Part.Part] == hash {
			continue
		}
		start := time.Now()
		if err := r2Put(bucket, chunk.Part.Key, string(chunk.Content)); err != nil {
			oplog.Log(oplog.OpR2Put, oplog.WithJobID(jobID),
				oplog.WithDetailf("live log part=%d", chunk.Part.Part),
				oplog.WithError(err), oplog.WithDuration(time.Since(start)))
			return
		}
		state.partHashes[chunk.Part.Part] = hash
		oplog.Log(oplog.OpR2Put, oplog.WithJobID(jobID),
			oplog.WithDetailf("live log part=%d", chunk.Part.Part),
			oplog.WithDuration(time.Since(start)))
	}

	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "encode live log manifest for job %d: %v\n", jobID, err)
		return
	}
	if state.manifestJSON == string(manifestJSON) {
		return
	}

	start := time.Now()
	if err := r2Put(bucket, cloudlog.ManifestKeyForRun(jobID, runID), string(manifestJSON)); err != nil {
		oplog.Log(oplog.OpR2Put, oplog.WithJobID(jobID),
			oplog.WithDetail("live log manifest"),
			oplog.WithError(err), oplog.WithDuration(time.Since(start)))
		return
	}
	state.manifestJSON = string(manifestJSON)
	oplog.Log(oplog.OpR2Put, oplog.WithJobID(jobID),
		oplog.WithDetail("live log manifest"), oplog.WithDuration(time.Since(start)))
}

func hashChunk(content []byte) uint64 {
	h := fnv.New64a()
	_, _ = h.Write(content)
	return h.Sum64()
}
