package dataloc

import (
	"database/sql"
	"fmt"
	"time"
)

type RequestStatus string

const (
	RequestPending   RequestStatus = "pending"
	RequestRunning   RequestStatus = "running"
	RequestCompleted RequestStatus = "completed"
	RequestFailed    RequestStatus = "failed"
)

type DataRequest struct {
	ID          int64
	Host        string
	Asset       DataAsset
	Revision    string
	Status      RequestStatus
	Error       string
	RemotePath  string
	SizeBytes   int64
	RequestedAt time.Time
	StartedAt   *time.Time
	CompletedAt *time.Time
}

type CreateRequestParams struct {
	Host     string
	Asset    DataAsset
	Revision string
}

func CreateDataRequest(db *sql.DB, params CreateRequestParams) (*DataRequest, error) {
	if params.Host == "" {
		return nil, fmt.Errorf("host is required")
	}
	if params.Asset.Kind == "" || params.Asset.ID == "" {
		return nil, fmt.Errorf("asset is required")
	}
	if params.Revision == "" {
		params.Revision = "main"
	}

	requestedAt := time.Now().UTC().Truncate(time.Second)
	result, err := db.Exec(`
		INSERT INTO data_requests (
			host, asset_kind, asset_id, revision, status, requested_at
		) VALUES (?, ?, ?, ?, ?, ?)
	`, params.Host, string(params.Asset.Kind), params.Asset.ID, params.Revision, string(RequestPending), requestedAt.Unix())
	if err != nil {
		return nil, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	return GetDataRequest(db, id)
}

func GetDataRequest(db *sql.DB, id int64) (*DataRequest, error) {
	row := db.QueryRow(`
		SELECT id, host, asset_kind, asset_id, revision, status, error_message,
			remote_path, size_bytes, requested_at, started_at, completed_at
		FROM data_requests
		WHERE id = ?
	`, id)
	request, err := scanDataRequest(rowScanner{row: row})
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return request, nil
}

func ListDataRequests(db *sql.DB, host string) ([]DataRequest, error) {
	query := `
		SELECT id, host, asset_kind, asset_id, revision, status, error_message,
			remote_path, size_bytes, requested_at, started_at, completed_at
		FROM data_requests
	`
	args := []any{}
	if host != "" {
		query += ` WHERE host = ?`
		args = append(args, host)
	}
	query += ` ORDER BY requested_at DESC, id DESC`
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var requests []DataRequest
	for rows.Next() {
		req, err := scanDataRequest(rowScanner{rows: rows})
		if err != nil {
			return nil, err
		}
		requests = append(requests, *req)
	}
	return requests, rows.Err()
}

func MarkDataRequestRunning(db *sql.DB, id int64) error {
	startedAt := time.Now().UTC().Truncate(time.Second)
	_, err := db.Exec(`
		UPDATE data_requests
		SET status = ?, started_at = ?, error_message = '', completed_at = NULL
		WHERE id = ?
	`, string(RequestRunning), startedAt.Unix(), id)
	return err
}

func MarkDataRequestCompleted(db *sql.DB, id int64, entry HostDataEntry) error {
	completedAt := time.Now().UTC().Truncate(time.Second)
	_, err := db.Exec(`
		UPDATE data_requests
		SET status = ?, error_message = '', remote_path = ?, size_bytes = ?, completed_at = ?
		WHERE id = ?
	`, string(RequestCompleted), entry.Path, entry.SizeBytes, completedAt.Unix(), id)
	return err
}

func MarkDataRequestFailed(db *sql.DB, id int64, errMessage string) error {
	completedAt := time.Now().UTC().Truncate(time.Second)
	_, err := db.Exec(`
		UPDATE data_requests
		SET status = ?, error_message = ?, completed_at = ?
		WHERE id = ?
	`, string(RequestFailed), errMessage, completedAt.Unix(), id)
	return err
}

type scanner interface {
	Scan(dest ...any) error
}

type rowScanner struct {
	row  *sql.Row
	rows *sql.Rows
}

func (s rowScanner) Scan(dest ...any) error {
	if s.row != nil {
		return s.row.Scan(dest...)
	}
	return s.rows.Scan(dest...)
}

func scanDataRequest(s scanner) (*DataRequest, error) {
	var req DataRequest
	var kind string
	var status string
	var requestedAt int64
	var startedAt sql.NullInt64
	var completedAt sql.NullInt64
	if err := s.Scan(
		&req.ID,
		&req.Host,
		&kind,
		&req.Asset.ID,
		&req.Revision,
		&status,
		&req.Error,
		&req.RemotePath,
		&req.SizeBytes,
		&requestedAt,
		&startedAt,
		&completedAt,
	); err != nil {
		return nil, err
	}
	req.Asset.Kind = AssetKind(kind)
	req.Status = RequestStatus(status)
	req.RequestedAt = time.Unix(requestedAt, 0).UTC()
	if startedAt.Valid {
		t := time.Unix(startedAt.Int64, 0).UTC()
		req.StartedAt = &t
	}
	if completedAt.Valid {
		t := time.Unix(completedAt.Int64, 0).UTC()
		req.CompletedAt = &t
	}
	return &req, nil
}
