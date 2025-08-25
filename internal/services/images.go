package services

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type ImageFetchArgs struct {
	EventID      int64  `json:"event_id"`
	UUID         string `json:"uuid"`
	BaseURL      string `json:"base_url"`      // e.g. https://police.openalpr.com
	AuthHeader   string `json:"auth_header"`   // e.g. "Authorization: Bearer xxx" or ""
	PathTemplate string `json:"path_template"` // e.g. "/img/{agent_uid}/{uuid}.jpeg?api_key=${REKOR_API_KEY}"
	AgentUID     string `json:"agent_uid"`     // provided by webhook payload
}

// StartImageWorker runs a simple polling worker to process image_fetch jobs
func StartImageWorker(ctx context.Context, db *sql.DB, dataDir string) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			processBatch(ctx, db, dataDir)
		}
	}
}

func processBatch(ctx context.Context, db *sql.DB, dataDir string) {
	// fetch up to 10 jobs
	rows, err := db.Query(`
		SELECT id, args_json
		FROM jobs
		WHERE type = 'image_fetch'
		  AND status IN ('queued','retry')
		  AND (next_run_at IS NULL OR next_run_at <= CURRENT_TIMESTAMP)
		LIMIT 10
	`)
	if err != nil {
		return
	}
	defer rows.Close()

	type jobRow struct {
		id   int64
		args string
	}
	var jobs []jobRow
	for rows.Next() {
		var jr jobRow
		_ = rows.Scan(&jr.id, &jr.args)
		jobs = append(jobs, jr)
	}

	for _, jr := range jobs {
		// mark processing (idempotent-ish)
		if _, err := db.Exec(`UPDATE jobs SET status='processing', updated_at=CURRENT_TIMESTAMP WHERE id=? AND status IN ('queued','retry')`, jr.id); err != nil {
			continue
		}
		var args ImageFetchArgs
		if err := json.Unmarshal([]byte(jr.args), &args); err != nil {
			failJob(db, jr.id, fmt.Errorf("args unmarshal: %w", err))
			continue
		}
		if err := fetchOne(ctx, db, args, dataDir); err != nil {
			failJob(db, jr.id, err)
		} else {
			_, _ = db.Exec(`UPDATE jobs SET status='done', updated_at=CURRENT_TIMESTAMP WHERE id=?`, jr.id)
		}
	}
}

func failJob(db *sql.DB, id int64, err error) {
	_, _ = db.Exec(`
		UPDATE jobs
		   SET status='retry',
		       attempts=attempts+1,
		       last_error=?,
		       next_run_at=datetime(CURRENT_TIMESTAMP, '+60 seconds'),
		       updated_at=CURRENT_TIMESTAMP
		 WHERE id=?`,
		strings.TrimSpace(err.Error()), id)
}

func fetchOne(ctx context.Context, db *sql.DB, args ImageFetchArgs, dataDir string) error {
	if args.UUID == "" || args.BaseURL == "" {
		return errors.New("missing uuid/base_url")
	}

	// Build the relative path from template if provided, else fallback
	rel := args.PathTemplate
	if strings.TrimSpace(rel) == "" {
		rel = "/img/{uuid}.jpg"
	}

	// substitute placeholders
	rel = strings.ReplaceAll(rel, "{uuid}", args.UUID)
	rel = strings.ReplaceAll(rel, "{agent_uid}", args.AgentUID)

	// expand ${ENV_VAR} tokens
	rel = expandEnvVars(rel)

	// build full URL
	base := strings.TrimRight(args.BaseURL, "/")
	if !strings.HasPrefix(rel, "/") {
		rel = "/" + rel
	}
	fullURL := base + rel

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return err
	}
	// Optional header, support either "Header: value" or a raw "Authorization: Bearer xxx"
	if h := strings.TrimSpace(args.AuthHeader); h != "" && !strings.HasPrefix(strings.ToLower(h), "env ") {
		if i := strings.Index(h, ":"); i > 0 {
			key := strings.TrimSpace(h[:i])
			val := strings.TrimSpace(h[i+1:])
			if key != "" && val != "" {
				req.Header.Set(key, val)
			}
		} else {
			req.Header.Set("Authorization", h)
		}
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("http get %s: %w", fullURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("bad status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	imgDir := filepath.Join(dataDir, "images")
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		return err
	}
	dst := filepath.Join(imgDir, args.UUID+".jpg")
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, resp.Body); err != nil {
		return err
	}

	_, _ = db.Exec(`UPDATE images SET path=?, status='stored' WHERE event_id=? AND uuid=?`, dst, args.EventID, args.UUID)
	return nil
}

func expandEnvVars(s string) string {
	// minimal ${VAR} expander
	out := s
	for {
		start := strings.Index(out, "${")
		if start < 0 {
			break
		}
		end := strings.Index(out[start:], "}")
		if end < 0 {
			break
		}
		end += start
		name := strings.TrimSpace(out[start+2 : end])
		val := os.Getenv(name)
		out = out[:start] + val + out[end+1:]
	}
	return out
}
