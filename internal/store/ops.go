package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// --- activity log ---

type Activity struct {
	ID        int64
	At        time.Time
	Level     string // info | warn | error
	Kind      string // tool | job | memory | model | character | system | approval | budget
	Character string
	UserID    int64
	Summary   string
	Detail    string
}

func (s *Store) LogActivity(ctx context.Context, a Activity) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO activity (at, level, kind, character, user_id, summary, detail) VALUES (?, ?, ?, ?, ?, ?, ?)",
		s.unix(), a.Level, a.Kind, a.Character, nullInt(a.UserID), s.clean(a.Summary), s.clean(a.Detail))
	return err
}

func (s *Store) RecentActivity(ctx context.Context, limit int, minLevel string) ([]Activity, error) {
	levels := []any{"info", "warn", "error"}
	switch minLevel {
	case "warn":
		levels = []any{"warn", "error", "error"}
	case "error":
		levels = []any{"error", "error", "error"}
	}
	return s.activity(ctx, `SELECT id, at, level, kind, character, COALESCE(user_id, 0), summary, detail FROM activity
		WHERE level IN (?, ?, ?) ORDER BY id DESC LIMIT ?`, append(levels, limit)...)
}

// UserActivity is the trail of what sprigs did for one user, newest first.
func (s *Store) UserActivity(ctx context.Context, userID int64, limit int) ([]Activity, error) {
	return s.activity(ctx, `SELECT id, at, level, kind, character, COALESCE(user_id, 0), summary, detail FROM activity
		WHERE user_id = ? ORDER BY id DESC LIMIT ?`, userID, limit)
}

// CharacterErrorsSince counts a sprig's logged errors since a time: how
// healthy it is, without saying what went wrong.
func (s *Store) CharacterErrorsSince(ctx context.Context, character string, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM activity WHERE character = ? AND level = 'error' AND at >= ?", character, since.Unix()).Scan(&n)
	return n, err
}

func (s *Store) activity(ctx context.Context, q string, args ...any) ([]Activity, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Activity
	for rows.Next() {
		var a Activity
		var at int64
		if err := rows.Scan(&a.ID, &at, &a.Level, &a.Kind, &a.Character, &a.UserID, &a.Summary, &a.Detail); err != nil {
			return nil, err
		}
		a.At = time.Unix(at, 0)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) PruneActivity(ctx context.Context, keep time.Duration) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM activity WHERE at < ?", s.unix()-int64(keep.Seconds()))
	return err
}

// --- usage & spend ---

type Usage struct {
	UserID           int64
	Character        string
	JobID            int64
	Purpose          string
	Origin           string // heartbeat | task
	Model            string
	PromptTokens     int
	CompletionTokens int
	CostUSD          float64
	// The prompt's make-up in characters (0 if not measured).
	PromptChars, HistoryChars, DocumentChars int
}

func (s *Store) RecordUsage(ctx context.Context, u Usage) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO usage (at, user_id, character, job_id, purpose, origin, model, prompt_tokens, completion_tokens, cost_usd,
		prompt_chars, history_chars, document_chars)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, s.unix(), nullInt(u.UserID), u.Character, nullInt(u.JobID), u.Purpose, u.Origin, u.Model,
		u.PromptTokens, u.CompletionTokens, u.CostUSD, u.PromptChars, u.HistoryChars, u.DocumentChars)
	return err
}

// SpendSince returns total spend since t, optionally filtered to a character.
func (s *Store) SpendSince(ctx context.Context, since time.Time, character string) (float64, error) {
	var v sql.NullFloat64
	var err error
	if character == "" {
		err = s.db.QueryRowContext(ctx, "SELECT SUM(cost_usd) FROM usage WHERE at >= ?", since.Unix()).Scan(&v)
	} else {
		err = s.db.QueryRowContext(ctx, "SELECT SUM(cost_usd) FROM usage WHERE at >= ? AND character = ?", since.Unix(), character).Scan(&v)
	}
	return v.Float64, err
}

type SpendRow struct {
	Key     string
	CostUSD float64
	Tokens  int
}

// SpendBreakdown groups spend since t by "character" or "job".
func (s *Store) SpendBreakdown(ctx context.Context, since time.Time, by string) ([]SpendRow, error) {
	col := "character"
	if by == "job" {
		col = "COALESCE(CAST(job_id AS TEXT), '')"
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+col+", SUM(cost_usd), SUM(prompt_tokens + completion_tokens) FROM usage WHERE at >= ? GROUP BY 1 ORDER BY 2 DESC", since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SpendRow
	for rows.Next() {
		var r SpendRow
		if err := rows.Scan(&r.Key, &r.CostUSD, &r.Tokens); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) JobSpendSince(ctx context.Context, jobID int64, since time.Time) (float64, error) {
	var v sql.NullFloat64
	err := s.db.QueryRowContext(ctx, "SELECT SUM(cost_usd) FROM usage WHERE job_id = ? AND at >= ?", jobID, since.Unix()).Scan(&v)
	return v.Float64, err
}

// --- settings ---

// VersionKey is the setting that holds the Thicket version that last ran
// this database: written at each start and by `thicket update`, which
// reads it to say what an update changes.
const VersionKey = "thicket.version"

func (s *Store) Setting(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

// SettingNames lists the stored settings whose names start with prefix.
func (s *Store) SettingNames(ctx context.Context, prefix string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT key FROM settings WHERE substr(key, 1, ?) = ? ORDER BY key", len(prefix), prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, key, value, s.unix())
	return err
}

func (s *Store) DeleteSetting(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM settings WHERE key = ?", key)
	return err
}

// --- encrypted secrets (ciphertext only; crypto lives in package secrets) ---

func (s *Store) PutSecretBlob(ctx context.Context, name string, nonce, ct []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO secrets (name, nonce, ciphertext, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET nonce = excluded.nonce, ciphertext = excluded.ciphertext, updated_at = excluded.updated_at`,
		name, nonce, ct, s.unix())
	return err
}

func (s *Store) SecretBlob(ctx context.Context, name string) (nonce, ct []byte, err error) {
	err = s.db.QueryRowContext(ctx, "SELECT nonce, ciphertext FROM secrets WHERE name = ?", name).Scan(&nonce, &ct)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	return nonce, ct, err
}

func (s *Store) SecretNames(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name FROM secrets ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ReplaceSecretBlobs rewrites stored secrets in one transaction (key
// rotation): every name given gets its new nonce and ciphertext.
func (s *Store) ReplaceSecretBlobs(ctx context.Context, blobs map[string][2][]byte) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.unix()
	for name, b := range blobs {
		res, err := tx.ExecContext(ctx, "UPDATE secrets SET nonce = ?, ciphertext = ?, updated_at = ? WHERE name = ?", b[0], b[1], now, name)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("secret %q changed during key rotation; nothing was changed, try again", name)
		}
	}
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM secrets").Scan(&n); err != nil {
		return err
	}
	if n != len(blobs) {
		return errors.New("a secret was added during key rotation; nothing was changed, try again")
	}
	return tx.Commit()
}

func (s *Store) DeleteSecret(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM secrets WHERE name = ?", name)
	return err
}

// --- MCP server registry ---

type MCPServer struct {
	Name      string            `json:"name"`
	Transport string            `json:"transport"` // stdio | http
	Target    string            `json:"target"`    // command or URL
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`     // values may contain {{secret:NAME}}
	Headers   map[string]string `json:"headers,omitempty"` // values may contain {{secret:NAME}}
	Enabled   bool              `json:"enabled"`
	// Exec marks a server that can run commands or programs: its tools
	// are offered only to sprigs with exec turned on.
	Exec bool `json:"exec,omitempty"`
	// Undo declares, per tool, the call that reverses it. A tool with no
	// entry is recorded in the trail as not undoable.
	Undo map[string]InverseCall `json:"undo,omitempty"`
}

// InverseCall is the call that undoes one of an integration's tools.
// Values in Args and Said may use {{args.NAME}} (what the tool was called
// with) and {{result.NAME}} (a field of its JSON result, or {{result}} for
// the whole text).
type InverseCall struct {
	Tool string            `json:"tool"`
	Args map[string]string `json:"args,omitempty"`
	// Said is how Thicket says what was done ("I added {{args.item}} to
	// {{args.list}}"); Label what undo does ("remove it").
	Said  string `json:"said,omitempty"`
	Label string `json:"label,omitempty"`
}

func (s *Store) PutMCPServer(ctx context.Context, m MCPServer) error {
	args, _ := json.Marshal(nonNilSlice(m.Args))
	env, _ := json.Marshal(nonNilMap(m.Env))
	hdr, _ := json.Marshal(nonNilMap(m.Headers))
	// No Undo map keeps what's stored (re-adding an integration doesn't
	// forget how to undo its tools); an empty one clears it.
	undo, _ := json.Marshal(m.Undo)
	if m.Undo == nil {
		undo = nil
	}
	en, ex := 0, 0
	if m.Enabled {
		en = 1
	}
	if m.Exec {
		ex = 1
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO mcp_servers (name, transport, target, args, env, headers, enabled, exec, undo, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET transport = excluded.transport, target = excluded.target, args = excluded.args,
		env = excluded.env, headers = excluded.headers, enabled = excluded.enabled, exec = excluded.exec,
		undo = CASE WHEN excluded.undo = '' THEN mcp_servers.undo ELSE excluded.undo END`,
		m.Name, m.Transport, m.Target, string(args), string(env), string(hdr), en, ex, string(undo), s.unix())
	if err == nil && undo == nil {
		_, err = s.db.ExecContext(ctx, "UPDATE mcp_servers SET undo = '{}' WHERE name = ? AND undo = ''", m.Name)
	}
	return err
}

func (s *Store) MCPServers(ctx context.Context) ([]MCPServer, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name, transport, target, args, env, headers, enabled, exec, undo FROM mcp_servers ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MCPServer
	for rows.Next() {
		var m MCPServer
		var args, env, hdr, undo string
		var en, ex int
		if err := rows.Scan(&m.Name, &m.Transport, &m.Target, &args, &env, &hdr, &en, &ex, &undo); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(args), &m.Args)
		_ = json.Unmarshal([]byte(env), &m.Env)
		_ = json.Unmarshal([]byte(hdr), &m.Headers)
		_ = json.Unmarshal([]byte(undo), &m.Undo)
		m.Enabled = en == 1
		m.Exec = ex == 1
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) DeleteMCPServer(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM mcp_servers WHERE name = ?", name)
	return err
}

func nonNilSlice(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func nonNilMap(v map[string]string) map[string]string {
	if v == nil {
		return map[string]string{}
	}
	return v
}

// --- approvals for risky actions ---

type Approval struct {
	ID        int64
	UserID    int64
	Character string
	ChatRef   string
	Tool      string
	Args      string
	Status    string
	Result    string
	CreatedAt time.Time
}

func (s *Store) CreateApproval(ctx context.Context, a Approval) (int64, error) {
	res, err := s.db.ExecContext(ctx, "INSERT INTO approvals (user_id, character, chat_ref, tool, args, status, created_at) VALUES (?, ?, ?, ?, ?, 'pending', ?)",
		a.UserID, a.Character, a.ChatRef, a.Tool, a.Args, s.unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// LatestPendingApproval returns the newest pending approval for a user in a
// chat, expiring any older than maxAge first.
func (s *Store) LatestPendingApproval(ctx context.Context, userID int64, chatRef string, maxAge time.Duration) (*Approval, error) {
	now := s.unix()
	if _, err := s.db.ExecContext(ctx, "UPDATE approvals SET status = 'expired', decided_at = ? WHERE status = 'pending' AND created_at < ?",
		now, now-int64(maxAge.Seconds())); err != nil {
		return nil, err
	}
	var a Approval
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT id, user_id, character, chat_ref, tool, args, status, result, created_at FROM approvals
		WHERE user_id = ? AND chat_ref = ? AND status = 'pending' ORDER BY id DESC LIMIT 1`, userID, chatRef).
		Scan(&a.ID, &a.UserID, &a.Character, &a.ChatRef, &a.Tool, &a.Args, &a.Status, &a.Result, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.CreatedAt = time.Unix(at, 0)
	return &a, nil
}

// DecideApproval moves a pending approval to approved/denied. It returns
// false if the approval was no longer pending (double-click safety).
func (s *Store) DecideApproval(ctx context.Context, id int64, status string) (bool, error) {
	res, err := s.db.ExecContext(ctx, "UPDATE approvals SET status = ?, decided_at = ? WHERE id = ? AND status = 'pending'", status, s.unix(), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (s *Store) SetApprovalResult(ctx context.Context, id int64, result string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE approvals SET result = ? WHERE id = ?", result, id)
	return err
}

// --- character file hashes ---

func (s *Store) CharacterFileHash(ctx context.Context, id string) (string, error) {
	var h string
	err := s.db.QueryRowContext(ctx, "SELECT sha256 FROM character_files WHERE id = ?", id).Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return h, err
}

func (s *Store) SetCharacterFileHash(ctx context.Context, id, hash string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO character_files (id, sha256, written_at) VALUES (?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET sha256 = excluded.sha256, written_at = excluded.written_at`, id, hash, s.unix())
	return err
}

// --- pending secret capture ---

type PendingSecret struct {
	UserID    int64
	Name      string
	Purpose   string
	CreatedAt time.Time
}

func (s *Store) SetPendingSecret(ctx context.Context, p PendingSecret) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO pending_secrets (user_id, name, purpose, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET name = excluded.name, purpose = excluded.purpose, created_at = excluded.created_at`,
		p.UserID, p.Name, p.Purpose, s.unix())
	return err
}

func (s *Store) PendingSecretFor(ctx context.Context, userID int64, maxAge time.Duration) (*PendingSecret, error) {
	var p PendingSecret
	var at int64
	err := s.db.QueryRowContext(ctx, "SELECT user_id, name, purpose, created_at FROM pending_secrets WHERE user_id = ?", userID).Scan(&p.UserID, &p.Name, &p.Purpose, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if s.unix()-at > int64(maxAge.Seconds()) {
		_ = s.ClearPendingSecret(ctx, userID)
		return nil, ErrNotFound
	}
	p.CreatedAt = time.Unix(at, 0)
	return &p, nil
}

func (s *Store) ClearPendingSecret(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM pending_secrets WHERE user_id = ?", userID)
	return err
}

// --- notices (de-duplication) ---

// ClaimOnce returns true the first time key is claimed, and false forever
// after.
func (s *Store) ClaimOnce(ctx context.Context, key string) (bool, error) {
	res, err := s.db.ExecContext(ctx, "INSERT INTO notices (key, sent_at) VALUES (?, ?) ON CONFLICT(key) DO NOTHING", key, s.unix())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ReleaseNotice drops a claim so the work can be tried again.
func (s *Store) ReleaseNotice(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM notices WHERE key = ?", key)
	return err
}

// ClaimNotice returns true if key hasn't been claimed within window, and
// claims it. Used so alerts and check-ins never spam.
func (s *Store) ClaimNotice(ctx context.Context, key string, window time.Duration) (bool, error) {
	now := s.unix()
	res, err := s.db.ExecContext(ctx, `INSERT INTO notices (key, sent_at) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET sent_at = excluded.sent_at WHERE notices.sent_at < ?`, key, now, now-int64(window.Seconds()))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}
