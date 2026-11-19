package store

import "context"

// ReachPrefs returns where a person asked to be reached: sprig id ("" for
// every sprig) -> channel.
func (s *Store) ReachPrefs(ctx context.Context, userID int64) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT character, channel FROM reach_prefs WHERE user_id = ?", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var c, ch string
		if err := rows.Scan(&c, &ch); err != nil {
			return nil, err
		}
		out[c] = ch
	}
	return out, rows.Err()
}

// SetReach records where a person wants proactive messages from a sprig
// ("" for every sprig). An empty channel goes back to the default.
func (s *Store) SetReach(ctx context.Context, userID int64, character, channel string) error {
	if channel == "" {
		_, err := s.db.ExecContext(ctx, "DELETE FROM reach_prefs WHERE user_id = ? AND character = ?", userID, character)
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO reach_prefs (user_id, character, channel, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (user_id, character) DO UPDATE SET channel = excluded.channel, updated_at = excluded.updated_at`, userID, character, channel, s.unix())
	return err
}
