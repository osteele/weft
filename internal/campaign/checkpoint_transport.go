package campaign

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
)

const checkpointTransportProbeTimeout = 20 * time.Second

var checkpointObjectExistsFunc = func(ctx context.Context, client *r2.Client, key string) (bool, error) {
	return client.ObjectExists(ctx, key)
}

var checkpointDigestFromHostFunc = dataloc.DigestPathOnHost
var checkpointR2PutFromHostFunc = dataloc.R2PutFromHost

var ErrCheckpointPublishDeferred = errors.New("checkpoint publish deferred")

type checkpointTransportState int

const (
	checkpointTransportUnknown checkpointTransportState = iota
	checkpointTransportOnHostOnly
	checkpointTransportR2Resident
)

type checkpointTransportInfo struct {
	state checkpointTransportState
	entry dataloc.HostDataEntry
	key   string
}

type CheckpointPublishResult struct {
	Uploaded        bool
	AlreadyResident bool
	Entry           dataloc.HostDataEntry
	Key             string
}

func resolveCheckpointTransport(ctx context.Context, database *sql.DB, client *r2.Client, asset dataloc.DataAsset) (checkpointTransportInfo, error) {
	if database == nil {
		return checkpointTransportInfo{state: checkpointTransportUnknown}, nil
	}
	entries, err := dataloc.FindAssetHosts(database, asset)
	if err != nil {
		return checkpointTransportInfo{state: checkpointTransportUnknown}, fmt.Errorf("find hosts for %s: %w", asset.Ref(), err)
	}
	if len(entries) == 0 {
		return checkpointTransportInfo{state: checkpointTransportOnHostOnly}, nil
	}
	entry := bestCheckpointEntry(entries)
	if entry.ContentHash == "" {
		return checkpointTransportInfo{state: checkpointTransportOnHostOnly, entry: entry}, nil
	}
	if client == nil {
		return checkpointTransportInfo{state: checkpointTransportUnknown, entry: entry}, nil
	}
	key := r2keys.NamedAsset(entry.ContentHash)
	checkCtx, cancel := context.WithTimeout(ctx, checkpointTransportProbeTimeout)
	defer cancel()
	exists, err := checkpointObjectExistsFunc(checkCtx, client, key)
	if err != nil {
		return checkpointTransportInfo{state: checkpointTransportUnknown, entry: entry, key: key}, err
	}
	if !exists {
		return checkpointTransportInfo{state: checkpointTransportOnHostOnly, entry: entry, key: key}, nil
	}
	return checkpointTransportInfo{state: checkpointTransportR2Resident, entry: entry, key: key}, nil
}

func bestCheckpointEntry(entries []dataloc.HostDataEntry) dataloc.HostDataEntry {
	best := entries[0]
	for _, entry := range entries[1:] {
		if entry.SizeBytes > best.SizeBytes {
			best = entry
		}
	}
	return best
}

func PublishCheckpointToR2(ctx context.Context, database *sql.DB, client *r2.Client, r2Cfg cloud.R2Config, asset dataloc.DataAsset) (CheckpointPublishResult, error) {
	if asset.Kind != dataloc.AssetCheckpoint {
		return CheckpointPublishResult{}, fmt.Errorf("asset %s is not a checkpoint", asset.Ref())
	}
	info, err := resolveCheckpointTransport(ctx, database, client, asset)
	if err != nil {
		return CheckpointPublishResult{}, fmt.Errorf("%w: resolve checkpoint transport for %s: %v", ErrCheckpointPublishDeferred, asset.Ref(), err)
	}
	result := CheckpointPublishResult{Entry: info.entry, Key: info.key}
	if info.state == checkpointTransportR2Resident {
		result.AlreadyResident = true
		return result, nil
	}
	if info.state == checkpointTransportUnknown {
		return result, fmt.Errorf("%w: checkpoint %s transportability is unknown", ErrCheckpointPublishDeferred, asset.Ref())
	}
	if info.entry.Host == "" || info.entry.Path == "" {
		return result, fmt.Errorf("%w: checkpoint %s has no holder path to publish", ErrCheckpointPublishDeferred, asset.Ref())
	}
	if info.entry.ContentHash == "" || info.entry.SizeBytes <= 0 {
		return result, fmt.Errorf("%w: checkpoint %s on %s lacks content hash/size; rerun `weft data add`", ErrCheckpointPublishDeferred, asset.Ref(), info.entry.Host)
	}
	contentType := info.entry.ContentType
	if contentType == "" {
		contentType = dataloc.ContentTypeDirectory
	}
	confirmed, err := checkpointDigestFromHostFunc(ctx, info.entry.Host, info.entry.Path)
	if err != nil {
		return result, fmt.Errorf("%w: confirm checkpoint %s on %s: %v", ErrCheckpointPublishDeferred, asset.Ref(), info.entry.Host, err)
	}
	if confirmed.Hash != info.entry.ContentHash {
		return result, fmt.Errorf("%w: checkpoint %s on %s hash changed: inventory has %s, holder reports %s; rerun `weft data add` before publishing",
			ErrCheckpointPublishDeferred, asset.Ref(), info.entry.Host, info.entry.ContentHash, confirmed.Hash)
	}
	if confirmed.SizeBytes != info.entry.SizeBytes {
		return result, fmt.Errorf("%w: checkpoint %s on %s size changed: inventory has %d, holder reports %d; rerun `weft data add` before publishing",
			ErrCheckpointPublishDeferred, asset.Ref(), info.entry.Host, info.entry.SizeBytes, confirmed.SizeBytes)
	}
	if confirmed.ContentType != "" && confirmed.ContentType != contentType {
		return result, fmt.Errorf("%w: checkpoint %s on %s content type changed: inventory has %s, holder reports %s; rerun `weft data add` before publishing",
			ErrCheckpointPublishDeferred, asset.Ref(), info.entry.Host, contentType, confirmed.ContentType)
	}
	if err := checkpointR2PutFromHostFunc(ctx, info.entry.Host, dataloc.R2PutFromHostRequest{
		Path:        info.entry.Path,
		Key:         info.key,
		ContentType: contentType,
		R2:          r2Cfg,
	}); err != nil {
		return result, err
	}
	result.Uploaded = true
	return result, nil
}

func resolveTransportableCheckpointNeeds(ctx context.Context, database *sql.DB, client *r2.Client, job *db.Job) ([]cloud.CloudNeed, error) {
	if job == nil {
		return nil, nil
	}
	var needs []cloud.CloudNeed
	for _, raw := range job.Inputs {
		asset, ok := dataloc.ParseAssetRef(raw)
		if !ok || asset.Kind != dataloc.AssetCheckpoint {
			continue
		}
		info, err := resolveCheckpointTransport(ctx, database, client, asset)
		if err != nil {
			return nil, fmt.Errorf("resolve transportability for %s on %s: %w", raw, ids.FormatJobID(job.ID), err)
		}
		if info.state != checkpointTransportR2Resident {
			continue
		}
		need, err := cloudNeedForTransportableCheckpoint(database, raw, asset, info)
		if err != nil {
			return nil, err
		}
		needs = append(needs, need)
	}
	return needs, nil
}

func cloudNeedForTransportableCheckpoint(database *sql.DB, spec string, asset dataloc.DataAsset, info checkpointTransportInfo) (cloud.CloudNeed, error) {
	targetPath := checkpointTargetPath(info.entry.Path, asset.ID)
	contentType := string(info.entry.ContentType)
	if contentType == "" {
		contentType = string(dataloc.ContentTypeDirectory)
	}
	if info.entry.ContentHash != "" {
		if named, err := db.GetNamedAssetByContentHash(database, info.entry.ContentHash); err == nil {
			targetPath = named.TargetPath
			contentType = named.ContentType
		} else if err != nil && !errors.Is(err, db.ErrNamedAssetNotFound) {
			return cloud.CloudNeed{}, fmt.Errorf("lookup named asset for %s: %w", spec, err)
		}
	}
	return cloud.CloudNeed{
		Spec:        spec,
		Path:        targetPath,
		R2Key:       info.key,
		ContentType: contentType,
	}, nil
}

func checkpointTargetPath(pathValue, assetID string) string {
	clean := strings.TrimSpace(pathValue)
	if clean == "" {
		return filepath.ToSlash(assetID)
	}
	if strings.HasPrefix(clean, "~/") || filepath.IsAbs(clean) {
		return filepath.Base(clean)
	}
	return filepath.ToSlash(clean)
}
