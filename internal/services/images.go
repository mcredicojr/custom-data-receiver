package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"database/sql"
)

type ImageFetchArgs struct {
	EventID    int64  `json:"event_id"`
	UUID       string `json:"uuid"`
	BaseURL    string `json:"base_url"`    // e.g. http://AGENT_IP:8355
	AuthHeader string `json:"auth_header"` // e.g. "Authorization: Bearer <token>" or ""
}

// StartImageWorker runs a simple polling worker to process image_fetch jobs
// It is safe to call in a goroutine; cancel ctx to stop.
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
	// Claim up to N queued jobs
	rows, err := db.Query(`
		SELECT id, args_json
		FROM jobs
		WHERE type = 'image_fetch'
		  AND status IN ('queued', 'retry')
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
		// mark processing
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
		err.Error(), id)
}

func fetchOne(ctx context.Context, db *sql.DB, args ImageFetchArgs, dataDir string) error {
	if args.UUID == "" || args.BaseURL == "" {
		return errors.New("missing uuid/base_url")
	}

	// Prepare URL like: http://AGENT:8355/img/UUID.jpg
	url := strings.TrimRight(args.BaseURL, "/") + "/img/" + args.UUID + ".jpg"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if strings.TrimSpace(args.AuthHeader) != "" {
		// Expect "Header-Name: value"
		parts := strings.SplitN(args.AuthHeader, ":", 2)
		if len(parts) == 2 {
			req.Header.Set(strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]))
		}
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("bad status %d: %s", resp.StatusCode, string(b))
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

	// Mark image row as stored with path
	_, _ = db.Exec(`UPDATE images SET path=?, status='stored' WHERE event_id=? AND uuid=?`, dst, args.EventID, args.UUID)
	return nil
}
