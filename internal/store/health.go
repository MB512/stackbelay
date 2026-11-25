package store

import (
	"context"
	"fmt"
	"time"

	"github.com/MB512/stackbelay/internal/jobs"
)

// StackHealth is the stack monitor's view of a stack (issue #49).
type StackHealth struct {
	StackID int64
	State   string    // confirmed state: started, degraded, stopped, unknown, missing
	Since   time.Time // when it was first observed
	Alerted bool      // an alert went out for the current bad state
	// Candidate is a different state seen in the last CandidateCount polls
	// in a row, first at CandidateSince; "" if none.
	Candidate      string
	CandidateSince time.Time
	CandidateCount int
	LastJobID      string // the newest finished job the monitor has seen
}

// StackHealths returns the monitor's view of all stacks it has seen.
func (s *Store) StackHealths(ctx context.Context) (map[int64]StackHealth, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT stack_id, state, since, alerted, candidate, candidate_since,
		candidate_count, last_job_id FROM stack_health`)
	if err != nil {
		return nil, fmt.Errorf("loading stack health: %w", err)
	}
	defer rows.Close()
	out := map[int64]StackHealth{}
	for rows.Next() {
		var h StackHealth
		var since, candSince string
		if err := rows.Scan(&h.StackID, &h.State, &since, &h.Alerted, &h.Candidate, &candSince,
			&h.CandidateCount, &h.LastJobID); err != nil {
			return nil, fmt.Errorf("loading stack health: %w", err)
		}
		if h.Since, err = parseTime(since); err != nil {
			return nil, err
		}
		if h.CandidateSince, err = parseOptionalTime(candSince); err != nil {
			return nil, err
		}
		out[h.StackID] = h
	}
	return out, rows.Err()
}

// SaveStackHealth inserts or replaces the monitor's view of a stack.
func (s *Store) SaveStackHealth(ctx context.Context, h StackHealth) error {
	candSince := ""
	if !h.CandidateSince.IsZero() {
		candSince = formatTime(h.CandidateSince)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO stack_health (stack_id, state, since, alerted, candidate, candidate_since, candidate_count, last_job_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (stack_id) DO UPDATE SET state = excluded.state, since = excluded.since, alerted = excluded.alerted,
		   candidate = excluded.candidate, candidate_since = excluded.candidate_since,
		   candidate_count = excluded.candidate_count, last_job_id = excluded.last_job_id`,
		h.StackID, h.State, formatTime(h.Since), h.Alerted, h.Candidate, candSince, h.CandidateCount, h.LastJobID)
	if err != nil {
		return fmt.Errorf("saving stack health of stack %d: %w", h.StackID, err)
	}
	return nil
}

// LatestFinishedJobs returns the newest finished job of each stack.
func (s *Store) LatestFinishedJobs(ctx context.Context) (map[int64]Job, error) {
	list, err := s.queryJobs(ctx, `SELECT `+jobColumns+jobFrom+
		`WHERE j.id IN (SELECT MAX(id) FROM jobs WHERE stack_id IS NOT NULL AND state IN (?, ?, ?) GROUP BY stack_id)`,
		jobs.StateSucceeded, jobs.StateFailed, jobs.StateCancelled)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]Job, len(list))
	for _, j := range list {
		out[j.StackID] = j
	}
	return out, nil
}
