package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"sync"
	"time"
)

const (
	buildTimeout = 75 * time.Second
	buildSlots   = 4
)

type buildSpec struct {
	Kind, Prompt, Character string
	App                     *savedApp `json:",omitempty"`
	Face                    *companionState `json:",omitempty"`
	FaceVersion             int64     `json:",omitempty"`
}

type buildJob struct {
	id, device, state, result string
	spec                      buildSpec
}

type buildManager struct {
	b       *bridge
	wake    chan struct{}
	mu      sync.Mutex
	workers map[string]buildWorker // owns both execution and session cleanup
	wg      sync.WaitGroup
}

type buildWorker struct {
	device string
	cancel context.CancelFunc
}

func newBuildManager(b *bridge) *buildManager {
	return &buildManager{b: b, wake: make(chan struct{}, 1), workers: make(map[string]buildWorker)}
}

func (m *buildManager) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *buildManager) enqueue(ctx context.Context, who peer, intent buildIntent, text string) (string, error) {
	if len(text) > 4096 {
		return "", problem(400, "Build requests can be at most 4096 bytes.")
	}
	id := strings.ToLower(who.DeviceID)
	spec := buildSpec{Kind: intent.kind, Prompt: text, Character: who.Character}
	switch intent.kind {
	case "edit":
		if intent.target == "" {
			h := m.b.ws
			if h != nil {
				h.mu.Lock()
				if d := h.devices[id]; d != nil && h.active(d) && d.app != nil && d.app.spec.Version > 0 {
					intent.target = d.app.spec.Name
				}
				h.mu.Unlock()
			}
		}
		if intent.target == "" {
			return "", problem(400, "Which saved app? Open it first or name its slug in backticks.")
		}
		a, err := m.b.loadApp(ctx, intent.target)
		if err != nil {
			return "", err
		}
		spec.App = &a
	case "face":
		tx, err := m.b.db.BeginTx(ctx, nil)
		if err != nil {
			return "", err
		}
		defer tx.Rollback()
		c, err := m.b.readActiveCompanion(ctx, tx, who)
		if err != nil {
			return "", err
		}
		spec.FaceVersion, err = faceVersion(ctx, tx, id)
		if err != nil {
			return "", err
		}
		// Only send appearance to the builder, not voice or personality.
		c.Voice, c.Personality = "", ""
		spec.Face = &c
		if err := tx.Commit(); err != nil {
			return "", err
		}
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return "", err
	}
	jobID := builderSessionPrefix + randomHex(16)
	if h := m.b.ws; h != nil {
		h.mu.Lock()
		defer h.mu.Unlock()
		if r := h.parentRefresh(id, time.Now()); r != "" {
			return "", r.err()
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	out, err := m.b.db.ExecContext(ctx, `INSERT INTO build_jobs (id,device_id,state,spec,created_at)
		SELECT ?,?,'queued',?,? WHERE NOT EXISTS
		(SELECT 1 FROM build_jobs WHERE device_id=? AND state IN ('queued','running'))
		AND (SELECT count(*) FROM build_jobs WHERE state IN ('queued','running')) < ?`,
		jobID, id, string(raw), time.Now().UnixMilli(), id, buildSlots)
	if err != nil {
		return "", err
	}
	n, err := out.RowsAffected()
	if n == 1 {
		m.signal()
		return jobID, err
	}
	return "", err
}

func (m *buildManager) cancel(ctx context.Context, device, job string) (bool, error) {
	var id string
	err := m.b.db.QueryRowContext(ctx, `UPDATE build_jobs SET state='cancelled',spec='',announced=1
		WHERE device_id=? AND (?='' OR id=?) AND state IN ('queued','running') RETURNING id`, strings.ToLower(device), job, job).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	m.mu.Lock()
	if stop := m.workers[id].cancel; stop != nil {
		stop()
	}
	m.mu.Unlock()
	m.signal()
	return true, nil
}

func (m *buildManager) run(ctx context.Context) {
	defer m.wg.Wait()
	recovered := false
	var pruneAt time.Time
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for ctx.Err() == nil {
		if !recovered {
			// An interrupted run never publishes a partially generated proposal.
			// Keep its session ID for cleanup; queued jobs have not sent a prompt.
			_, err := m.b.db.ExecContext(ctx, `UPDATE build_jobs SET state='failed',spec='',result=?
				WHERE state='running'`, "The bridge restarted. Ask me to build it again.")
			recovered = err == nil
		}
		if recovered {
			now := time.Now()
			if !now.Before(pruneAt) {
				_, _ = m.b.db.ExecContext(ctx, `DELETE FROM build_jobs WHERE state NOT IN ('queued','running')
					AND cleanup=0 AND created_at<?`, now.Add(-7*24*time.Hour).UnixMilli())
				pruneAt = now.Add(time.Hour)
			}
			m.pump(ctx, now)
		}
		select {
		case <-ctx.Done():
		case <-tick.C:
		case <-m.wake:
		}
	}
}

func (m *buildManager) pump(ctx context.Context, now time.Time) {
	rows, err := m.b.db.readDB().QueryContext(ctx, `SELECT id,device_id,state,spec,result,announced,cleanup,created_at FROM build_jobs
		WHERE state='queued' OR (state<>'running' AND (announced=0 OR cleanup=1)) ORDER BY created_at,id`)
	if err != nil {
		return
	}
	type pending struct {
		buildJob
		raw                string
		announced, cleanup bool
		createdAt          int64
	}
	var list []pending
	for rows.Next() {
		var p pending
		if rows.Scan(&p.id, &p.device, &p.state, &p.raw, &p.result, &p.announced, &p.cleanup, &p.createdAt) == nil {
			list = append(list, p)
		}
	}
	_ = rows.Close()
	for _, p := range list {
		if p.state == "queued" {
			if p.createdAt < now.Add(-3*time.Minute).UnixMilli() {
				m.fail(ctx, p.id, "The build expired. Ask me to build it again.")
				continue
			}
			if json.Unmarshal([]byte(p.raw), &p.spec) != nil {
				m.fail(ctx, p.id, "I couldn't read that build request.")
				continue
			}
			m.start(ctx, p.buildJob, false)
		} else {
			if !p.announced && m.b.ws != nil {
				m.b.ws.announceBuild(ctx, p.buildJob)
			}
			if p.cleanup {
				m.start(ctx, p.buildJob, true)
			}
		}
	}
}

func (m *buildManager) start(ctx context.Context, job buildJob, cleanup bool) {
	m.mu.Lock()
	if len(m.workers) >= buildSlots || m.workers[job.id].cancel != nil {
		m.mu.Unlock()
		return
	}
	if !cleanup {
		for _, w := range m.workers {
			if w.device == job.device {
				m.mu.Unlock()
				return
			}
		}
	}
	run, cancel := context.WithTimeout(ctx, buildTimeout)
	worker := buildWorker{cancel: cancel}
	if !cleanup {
		worker.device = job.device
	}
	m.workers[job.id] = worker
	m.mu.Unlock()
	m.wg.Go(func() {
		claimed := false
		defer func() {
			cancel()
			m.mu.Lock()
			delete(m.workers, job.id)
			m.mu.Unlock()
			if claimed {
				m.signal()
			}
		}()
		if cleanup {
			m.cleanup(ctx, job)
			return
		}
		claimed = m.claim(run, job)
		if !claimed {
			return
		}
		err := m.build(run, job)
		if err != nil && ctx.Err() == nil {
			message := "I couldn't finish that build. Try asking again."
			switch {
			case errors.Is(err, errStaleBuild):
				message = "Something changed while I worked. I kept the newer version."
			case errors.Is(run.Err(), context.DeadlineExceeded):
				message = "That build took too long. Try a smaller change."
			}
			m.fail(ctx, job.id, message)
		}
		// On shutdown, recovery will report the running job and retry cleanup.
		if ctx.Err() == nil {
			m.cleanup(ctx, job)
		}
	})
}

func (m *buildManager) claim(ctx context.Context, job buildJob) bool {
	if h := m.b.ws; h != nil {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.parentRefresh(job.device, time.Now()) != "" {
			return false
		}
	}
	var id string
	return m.b.db.QueryRowContext(ctx, `UPDATE build_jobs SET state='running',cleanup=1
		WHERE id=? AND state='queued' RETURNING id`, job.id).Scan(&id) == nil
}

func (m *buildManager) fail(ctx context.Context, id, message string) {
	if _, err := m.b.db.ExecContext(ctx, `UPDATE build_jobs SET state='failed',spec='',result=?
		WHERE id=? AND state IN ('queued','running')`, message, id); err != nil && ctx.Err() == nil {
		log.Print("Lark could not settle a build job")
	}
}

func faceVersion(ctx context.Context, q companionQuery, id string) (version int64, err error) {
	err = q.QueryRowContext(ctx, `SELECT COALESCE((SELECT updated_at FROM companions WHERE device_id=?),0)`, id).Scan(&version)
	return
}
