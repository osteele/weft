package cloudlog

import (
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/r2keys"
)

const (
	DefaultChunkTargetBytes = 4 * 1024 * 1024
)

type Manifest struct {
	Version          int    `json:"version"`
	UpdatedAt        int64  `json:"updated_at"`
	ChunkTargetBytes int    `json:"chunk_target_bytes"`
	TotalBytes       int    `json:"total_bytes"`
	TotalLines       int    `json:"total_lines"`
	Parts            []Part `json:"parts"`
}

type Part struct {
	Part      int    `json:"part"`
	Key       string `json:"key"`
	SizeBytes int    `json:"size_bytes"`
	LineCount int    `json:"line_count"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Sealed    bool   `json:"sealed"`
}

type Chunk struct {
	Part    Part
	Content []byte
}

func BuildChunks(jobID int64, content []byte, targetBytes int) (Manifest, []Chunk) {
	return BuildRunChunks(jobID, 0, content, targetBytes)
}

func BuildRunChunks(jobID, runID int64, content []byte, targetBytes int) (Manifest, []Chunk) {
	if targetBytes <= 0 {
		targetBytes = DefaultChunkTargetBytes
	}

	manifest := Manifest{
		Version:          1,
		UpdatedAt:        time.Now().Unix(),
		ChunkTargetBytes: targetBytes,
		TotalBytes:       len(content),
	}
	if len(content) == 0 {
		return manifest, nil
	}

	var chunks []Chunk
	start := 0
	nextLine := 1
	partNum := 1

	for start < len(content) {
		end := len(content)
		sealed := false
		if end-start > targetBytes {
			end = start + splitIndex(content[start:], targetBytes)
			sealed = true
		}

		partContent := append([]byte(nil), content[start:end]...)
		lineCount := CountLines(partContent)
		part := Part{
			Part:      partNum,
			Key:       r2keys.JobAttemptLiveLogPart(jobID, runID, partNum),
			SizeBytes: len(partContent),
			LineCount: lineCount,
			StartLine: nextLine,
			EndLine:   nextLine + lineCount - 1,
			Sealed:    sealed,
		}
		if lineCount == 0 {
			part.EndLine = nextLine - 1
		}
		chunks = append(chunks, Chunk{Part: part, Content: partContent})
		manifest.Parts = append(manifest.Parts, part)
		manifest.TotalLines += lineCount
		nextLine += lineCount
		start = end
		partNum++
	}

	if len(manifest.Parts) > 0 {
		manifest.Parts[len(manifest.Parts)-1].Sealed = false
		chunks[len(chunks)-1].Part.Sealed = false
	}

	return manifest, chunks
}

func CountLines(content []byte) int {
	if len(content) == 0 {
		return 0
	}
	count := strings.Count(string(content), "\n")
	if content[len(content)-1] != '\n' {
		count++
	}
	return count
}

func SelectParts(manifest Manifest, from, to, lines int) []Part {
	if len(manifest.Parts) == 0 {
		return nil
	}
	if from > 0 || to > 0 {
		startLine := 1
		if from > 0 {
			startLine = from
		}
		endLine := manifest.TotalLines
		if to > 0 {
			endLine = to
		}
		if endLine < startLine {
			endLine = startLine
		}
		var selected []Part
		for _, part := range manifest.Parts {
			if part.LineCount == 0 {
				continue
			}
			if part.EndLine < startLine || part.StartLine > endLine {
				continue
			}
			selected = append(selected, part)
		}
		return selected
	}

	if lines <= 0 {
		lines = 50
	}
	remaining := lines
	selected := make([]Part, 0, len(manifest.Parts))
	for i := len(manifest.Parts) - 1; i >= 0; i-- {
		part := manifest.Parts[i]
		selected = append([]Part{part}, selected...)
		remaining -= part.LineCount
		if remaining <= 0 {
			break
		}
	}
	return selected
}

func AdjustQueryForParts(parts []Part, from, to, lines int) (int, int, int) {
	if len(parts) == 0 {
		return 0, 0, lines
	}
	if from == 0 && to == 0 {
		return 0, 0, lines
	}
	base := parts[0].StartLine
	if base <= 0 {
		base = 1
	}
	localFrom := 0
	localTo := 0
	if from > 0 {
		localFrom = from - base + 1
		if localFrom < 1 {
			localFrom = 1
		}
	}
	if to > 0 {
		localTo = to - base + 1
		if localTo < 1 {
			localTo = 1
		}
	}
	return localFrom, localTo, lines
}

func splitIndex(content []byte, targetBytes int) int {
	if len(content) <= targetBytes {
		return len(content)
	}
	if targetBytes <= 0 {
		return len(content)
	}

	limit := targetBytes
	if limit > len(content) {
		limit = len(content)
	}
	for i := limit; i < len(content); i++ {
		if content[i] == '\n' {
			return i + 1
		}
	}
	for i := limit - 1; i >= 0; i-- {
		if content[i] == '\n' {
			return i + 1
		}
	}
	return len(content)
}

func ManifestKey(jobID int64) string {
	return ManifestKeyForRun(jobID, 0)
}

func ManifestKeyForRun(jobID, runID int64) string {
	return r2keys.JobAttemptLiveLogManifest(jobID, runID)
}

func Prefix(jobID int64) string {
	return PrefixForRun(jobID, 0)
}

func PrefixForRun(jobID, runID int64) string {
	return r2keys.JobAttemptLiveLogsPrefix(jobID, runID)
}

func Summary(m Manifest) string {
	return fmt.Sprintf("%d parts, %d bytes, %d lines", len(m.Parts), m.TotalBytes, m.TotalLines)
}
