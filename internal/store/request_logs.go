package store

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

type RequestLog struct {
	ID           string
	Timestamp    time.Time
	UserID       string
	Username     string // joined for display
	ServerID     string
	ServerName   string // joined for display
	Method       string
	EndpointName string
	DurationMs   int64
	Status       string // "success", "error", "denied"
	ErrorMsg     string
}

type LogStats struct {
	TotalRequests24h int
	Errors24h        int
	AvgDurationMs    int
	ByServer         []ServerRequestCount
}

type ServerRequestCount struct {
	ServerName string
	Count      int
}

type EndpointCallCount struct {
	EndpointName string
	CallCount    int
	ErrorCount   int
	AvgDurationMs int
}

type RequestLogStore struct {
	db *DB
}

func NewRequestLogStore(db *DB) *RequestLogStore {
	return &RequestLogStore{db: db}
}

func (s *RequestLogStore) Create(rl *RequestLog) error {
	if rl.ID == "" {
		rl.ID = uuid.New().String()
	}
	if rl.Timestamp.IsZero() {
		rl.Timestamp = time.Now()
	}

	_, err := s.db.Exec(`
		INSERT INTO request_logs (id, timestamp, user_id, server_id, method, endpoint_name, duration_ms, status, error_msg)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rl.ID, rl.Timestamp, rl.UserID, rl.ServerID, rl.Method, rl.EndpointName, rl.DurationMs, rl.Status, rl.ErrorMsg,
	)
	if err != nil {
		return fmt.Errorf("creating request log: %w", err)
	}
	return nil
}

func (s *RequestLogStore) Recent(limit int) ([]*RequestLog, error) {
	rows, err := s.db.Query(`
		SELECT rl.id, rl.timestamp, rl.user_id, COALESCE(u.username, ''), rl.server_id, COALESCE(sv.name, ''),
		       rl.method, COALESCE(rl.endpoint_name, ''), COALESCE(rl.duration_ms, 0), COALESCE(rl.status, ''), COALESCE(rl.error_msg, '')
		FROM request_logs rl
		LEFT JOIN users u ON rl.user_id = u.id
		LEFT JOIN servers sv ON rl.server_id = sv.id
		ORDER BY rl.timestamp DESC
		LIMIT ?`, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("querying recent logs: %w", err)
	}
	defer rows.Close()

	return scanLogs(rows)
}

func (s *RequestLogStore) ByServer(serverID string, limit int) ([]*RequestLog, error) {
	rows, err := s.db.Query(`
		SELECT rl.id, rl.timestamp, rl.user_id, COALESCE(u.username, ''), rl.server_id, COALESCE(sv.name, ''),
		       rl.method, COALESCE(rl.endpoint_name, ''), COALESCE(rl.duration_ms, 0), COALESCE(rl.status, ''), COALESCE(rl.error_msg, '')
		FROM request_logs rl
		LEFT JOIN users u ON rl.user_id = u.id
		LEFT JOIN servers sv ON rl.server_id = sv.id
		WHERE rl.server_id = ?
		ORDER BY rl.timestamp DESC
		LIMIT ?`, serverID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("querying server logs: %w", err)
	}
	defer rows.Close()

	return scanLogs(rows)
}

func (s *RequestLogStore) Stats() (*LogStats, error) {
	stats := &LogStats{}

	// Total requests and errors in last 24h
	err := s.db.QueryRow(`
		SELECT COUNT(*), COALESCE(SUM(CASE WHEN status='error' THEN 1 ELSE 0 END), 0),
		       COALESCE(AVG(duration_ms), 0)
		FROM request_logs
		WHERE timestamp >= datetime('now', '-24 hours')`,
	).Scan(&stats.TotalRequests24h, &stats.Errors24h, &stats.AvgDurationMs)
	if err != nil {
		return stats, fmt.Errorf("querying log stats: %w", err)
	}

	// Requests by server in last 24h
	rows, err := s.db.Query(`
		SELECT COALESCE(sv.name, 'unknown'), COUNT(*)
		FROM request_logs rl
		LEFT JOIN servers sv ON rl.server_id = sv.id
		WHERE rl.timestamp >= datetime('now', '-24 hours')
		GROUP BY rl.server_id
		ORDER BY COUNT(*) DESC`,
	)
	if err != nil {
		return stats, nil
	}
	defer rows.Close()

	for rows.Next() {
		var sc ServerRequestCount
		if err := rows.Scan(&sc.ServerName, &sc.Count); err != nil {
			continue
		}
		stats.ByServer = append(stats.ByServer, sc)
	}

	return stats, nil
}

func (s *RequestLogStore) EndpointCounts(serverID string) ([]EndpointCallCount, error) {
	rows, err := s.db.Query(`
		SELECT endpoint_name, COUNT(*), COUNT(CASE WHEN status='error' THEN 1 END), COALESCE(AVG(duration_ms), 0)
		FROM request_logs
		WHERE server_id = ? AND endpoint_name IS NOT NULL AND endpoint_name != ''
		GROUP BY endpoint_name`, serverID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying endpoint counts: %w", err)
	}
	defer rows.Close()

	var counts []EndpointCallCount
	for rows.Next() {
		var ec EndpointCallCount
		if err := rows.Scan(&ec.EndpointName, &ec.CallCount, &ec.ErrorCount, &ec.AvgDurationMs); err != nil {
			continue
		}
		counts = append(counts, ec)
	}
	return counts, nil
}

func (s *RequestLogStore) ServerTotalCounts() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT server_id, COUNT(*) FROM request_logs GROUP BY server_id`)
	if err != nil {
		return nil, fmt.Errorf("querying server total counts: %w", err)
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var serverID string
		var count int
		if err := rows.Scan(&serverID, &count); err != nil {
			continue
		}
		counts[serverID] = count
	}
	return counts, nil
}

func scanLogs(rows interface {
	Next() bool
	Scan(dest ...any) error
}) ([]*RequestLog, error) {
	var logs []*RequestLog
	for rows.Next() {
		rl := &RequestLog{}
		if err := rows.Scan(
			&rl.ID, &rl.Timestamp, &rl.UserID, &rl.Username, &rl.ServerID, &rl.ServerName,
			&rl.Method, &rl.EndpointName, &rl.DurationMs, &rl.Status, &rl.ErrorMsg,
		); err != nil {
			return nil, fmt.Errorf("scanning request log: %w", err)
		}
		logs = append(logs, rl)
	}
	return logs, nil
}
