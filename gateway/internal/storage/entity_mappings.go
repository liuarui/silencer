package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/go-sql-driver/mysql"
	"github.com/mattn/go-sqlite3"
)

// Entity mappings are protocol state, independent of optional diagnostic logs.
// They deliberately have no TTL: forgetting an old token breaks a conversation.
type EntityMapping struct {
	Token    string `json:"token"`
	Original string `json:"original"`
	Label    string `json:"label"`
}

var entityTokenPattern = regexp.MustCompile(`^\{\{([A-Z0-9_]{1,12})_([a-f0-9]{16}|[bcdfghjkmnpqrstvwxz]{6})\}\}$`)
var entityScopePattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var entityLabelPattern = regexp.MustCompile(`^[A-Z0-9_]{1,12}$`)

func duplicateEntity(err error) bool {
	var my *mysql.MySQLError
	if errors.As(err, &my) {
		return my.Number == 1062
	}
	var lite sqlite3.Error
	return errors.As(err, &lite) && (lite.ExtendedCode == sqlite3.ErrConstraintPrimaryKey || lite.ExtendedCode == sqlite3.ErrConstraintUnique)
}

func entityKey(original, label string) string {
	sum := sha256.Sum256([]byte(label + "\x00" + original))
	return hex.EncodeToString(sum[:])
}
func (s *Storage) initEntityMappings(ctx context.Context, blob string) error {
	for _, statement := range []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS entity_mapping_tokens (scope VARCHAR(64) NOT NULL,token VARCHAR(64) NOT NULL,suffix VARCHAR(16) NOT NULL,label_key VARCHAR(12) NOT NULL,label VARCHAR(12) NOT NULL,entity_key VARCHAR(64) NOT NULL,original %s NOT NULL,ambiguous INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(scope,token))`, blob),
		`CREATE TABLE IF NOT EXISTS entity_mapping_entities (scope VARCHAR(64) NOT NULL,entity_key VARCHAR(64) NOT NULL,token VARCHAR(64) NOT NULL,PRIMARY KEY(scope,entity_key))`,
		`CREATE TABLE IF NOT EXISTS entity_mapping_migrations (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE IF NOT EXISTS entity_mapping_groups (scope VARCHAR(64) NOT NULL,label_key VARCHAR(12) NOT NULL,suffix VARCHAR(16) NOT NULL,PRIMARY KEY(scope,label_key,suffix))`,
	} {
		if s.mode == "mysql" {
			statement += " ENGINE=InnoDB"
		}
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	index := `CREATE INDEX IF NOT EXISTS idx_entity_mapping_suffix ON entity_mapping_tokens(scope,label_key,suffix)`
	if s.mode == "mysql" {
		index = `CREATE INDEX idx_entity_mapping_suffix ON entity_mapping_tokens(scope,label_key,suffix)`
	}
	if _, err := s.db.ExecContext(ctx, index); err != nil && !isAlreadyExistsError(err) {
		return err
	}
	return nil
}
func scanEntity(row interface{ Scan(...any) error }) (EntityMapping, error) {
	var m EntityMapping
	var original []byte
	err := row.Scan(&m.Token, &original, &m.Label)
	m.Original = string(original)
	return m, err
}

const canonicalEntityQuery = `SELECT t.token,t.original,t.label FROM entity_mapping_entities e JOIN entity_mapping_tokens t ON t.scope=e.scope AND t.token=e.token WHERE e.scope=? AND e.entity_key=? AND t.ambiguous=0`

func (s *Storage) IssueEntityMapping(ctx context.Context, scope, original, label string) (EntityMapping, error) {
	if !entityScopePattern.MatchString(scope) || !entityLabelPattern.MatchString(label) {
		return EntityMapping{}, errors.New("invalid entity mapping identity")
	}
	key := entityKey(original, label)
	found, err := scanEntity(s.db.QueryRowContext(ctx, canonicalEntityQuery, scope, key))
	if err == nil {
		if found.Original != original || found.Label != label {
			return EntityMapping{}, errors.New("entity mapping identity conflict")
		}
		return found, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return EntityMapping{}, err
	}
	var result EntityMapping
	err = s.enqueueAndWait(ctx, func(ctx context.Context, db *sql.DB) error {
		// The unique database entity key, rather than a process-local lock, chooses
		// the winner when two gateways issue the same entity concurrently.
		for attempt := 0; attempt < 8; attempt++ {
			var entropy [8]byte
			if _, err := rand.Read(entropy[:]); err != nil {
				return err
			}
			token := "{{" + label + "_" + hex.EncodeToString(entropy[:]) + "}}"
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO entity_mapping_tokens(scope,token,suffix,label_key,label,entity_key,original) VALUES(?,?,?,?,?,?,?)`, scope, token, hex.EncodeToString(entropy[:]), strings.ReplaceAll(label, "_", ""), label, key, []byte(original))
			if err != nil {
				tx.Rollback()
				if duplicateEntity(err) {
					continue
				}
				return err
			}
			insert := `INSERT INTO entity_mapping_entities(scope,entity_key,token) VALUES(?,?,?) ON CONFLICT(scope,entity_key) DO NOTHING`
			if s.mode == "mysql" {
				insert = `INSERT INTO entity_mapping_entities(scope,entity_key,token) VALUES(?,?,?) ON DUPLICATE KEY UPDATE entity_key=VALUES(entity_key)`
			}
			if _, err = tx.ExecContext(ctx, insert, scope, key, token); err != nil {
				tx.Rollback()
				return err
			}
			result, err = scanEntity(tx.QueryRowContext(ctx, canonicalEntityQuery, scope, key))
			if err == nil && (result.Original != original || result.Label != label) {
				err = errors.New("entity mapping identity conflict")
			}
			if err == nil && result.Token != token {
				_, err = tx.ExecContext(ctx, `DELETE FROM entity_mapping_tokens WHERE scope=? AND token=?`, scope, token)
			}
			if err != nil {
				tx.Rollback()
				return err
			}
			return tx.Commit()
		}
		return errors.New("entity token collision limit")
	})
	return result, err
}
func (s *Storage) ResolveEntityMapping(ctx context.Context, scope, token string) (EntityMapping, bool, error) {
	parts := entityTokenPattern.FindStringSubmatch(token)
	if !entityScopePattern.MatchString(scope) || len(parts) != 3 {
		return EntityMapping{}, false, nil
	}
	// A model may remove label underscores. Only accept this spelling when all
	// matching records agree on the exact original. Never guess by suffix alone.
	rows, err := s.db.QueryContext(ctx, `SELECT token,original,label,ambiguous FROM entity_mapping_tokens WHERE scope=? AND label_key=? AND suffix=?`, scope, strings.ReplaceAll(parts[1], "_", ""), parts[2])
	if err != nil {
		return EntityMapping{}, false, err
	}
	defer rows.Close()
	var result EntityMapping
	found := false
	for rows.Next() {
		var m EntityMapping
		var original []byte
		var ambiguous int
		if err = rows.Scan(&m.Token, &original, &m.Label, &ambiguous); err != nil {
			return EntityMapping{}, false, err
		}
		m.Original = string(original)
		if ambiguous != 0 || (found && result.Original != m.Original) {
			return EntityMapping{}, false, errors.New("ambiguous entity mapping")
		}
		result = m
		found = true
	}
	return result, found, rows.Err()
}

// Import existing entity-v1 evidence once, in bounded pages with no age/row
// cutoff. Structural legacy slots are deliberately excluded.
func (s *Storage) migrateEntityMappings(ctx context.Context) error {
	var complete int
	err := s.db.QueryRowContext(ctx, `SELECT id FROM entity_mapping_migrations WHERE id=1`).Scan(&complete)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return s.backfillEntityMappings(ctx)
}

func (s *Storage) backfillEntityMappings(ctx context.Context) error {
	cursor := ""
	for {
		rows, err := s.db.QueryContext(ctx, `SELECT id,meta FROM events WHERE id>? AND event_type='MASK' AND meta LIKE ? ORDER BY id LIMIT 128`, cursor, `%entity-v1%`)
		if err != nil {
			return err
		}
		type record struct{ id, meta string }
		var page []record
		for rows.Next() {
			var r record
			if err = rows.Scan(&r.id, &r.meta); err != nil {
				break
			}
			page = append(page, r)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		if len(page) == 0 {
			break
		}
		for _, r := range page {
			cursor = r.id
			var meta struct {
				Scope   string `json:"mapping_scope"`
				Mode    string `json:"mapping_mode"`
				Expired bool   `json:"log_expired"`
				Items   []struct {
					Token    string  `json:"preview"`
					Original *string `json:"original"`
				} `json:"items"`
			}
			if json.Unmarshal([]byte(r.meta), &meta) != nil || meta.Mode != "entity-v1" || meta.Expired || !entityScopePattern.MatchString(meta.Scope) {
				continue
			}
			for _, item := range meta.Items {
				parts := entityTokenPattern.FindStringSubmatch(item.Token)
				// Only the entity-v1 six-consonant format had a trustworthy global identity.
				if len(parts) != 3 || len(parts[2]) != 6 || item.Original == nil {
					continue
				}
				if err = s.importEntityMapping(ctx, meta.Scope, EntityMapping{Token: item.Token, Original: *item.Original, Label: parts[1]}); err != nil {
					return err
				}
			}
		}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO entity_mapping_migrations(id) VALUES(1)`)
	if duplicateEntity(err) {
		return nil
	}
	return err
}
func (s *Storage) importEntityMapping(ctx context.Context, scope string, m EntityMapping) error {
	return s.importEntityMappingState(ctx, scope, m, false)
}

func (s *Storage) importEntityMappingState(ctx context.Context, scope string, m EntityMapping, forceAmbiguous bool) error {
	parts := entityTokenPattern.FindStringSubmatch(m.Token)
	if !entityScopePattern.MatchString(scope) || len(parts) != 3 || parts[1] != m.Label {
		return errors.New("invalid imported entity mapping")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	key := entityKey(m.Original, m.Label)
	labelKey := strings.ReplaceAll(m.Label, "_", "")
	// Serialize even an initially empty alias group across database clients.
	// The first consistent read below occurs only after this write lock.
	group := `INSERT INTO entity_mapping_groups(scope,label_key,suffix) VALUES(?,?,?) ON CONFLICT(scope,label_key,suffix) DO NOTHING`
	if s.mode == "mysql" {
		group = `INSERT INTO entity_mapping_groups(scope,label_key,suffix) VALUES(?,?,?) ON DUPLICATE KEY UPDATE label_key=VALUES(label_key)`
	}
	if _, err = tx.ExecContext(ctx, group, scope, labelKey, parts[2]); err != nil {
		return err
	}
	insert := `INSERT INTO entity_mapping_tokens(scope,token,suffix,label_key,label,entity_key,original) VALUES(?,?,?,?,?,?,?) ON CONFLICT(scope,token) DO NOTHING`
	if s.mode == "mysql" {
		insert = `INSERT INTO entity_mapping_tokens(scope,token,suffix,label_key,label,entity_key,original) VALUES(?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE token=VALUES(token)`
	}
	if _, err = tx.ExecContext(ctx, insert, scope, m.Token, parts[2], strings.ReplaceAll(m.Label, "_", ""), m.Label, key, []byte(m.Original)); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT original,ambiguous FROM entity_mapping_tokens WHERE scope=? AND label_key=? AND suffix=?`, scope, labelKey, parts[2])
	if err != nil {
		return err
	}
	ambiguous := forceAmbiguous
	for rows.Next() {
		var previous []byte
		var flag int
		if err = rows.Scan(&previous, &flag); err != nil {
			break
		}
		ambiguous = ambiguous || flag != 0 || !bytes.Equal(previous, []byte(m.Original))
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	if ambiguous {
		if _, err = tx.ExecContext(ctx, `DELETE FROM entity_mapping_entities WHERE scope=? AND token IN (SELECT token FROM entity_mapping_tokens WHERE scope=? AND label_key=? AND suffix=?)`, scope, scope, labelKey, parts[2]); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE entity_mapping_tokens SET ambiguous=1 WHERE scope=? AND label_key=? AND suffix=?`, scope, labelKey, parts[2]); err != nil {
			return err
		}
	} else {
		insert = `INSERT INTO entity_mapping_entities(scope,entity_key,token) VALUES(?,?,?) ON CONFLICT(scope,entity_key) DO NOTHING`
		if s.mode == "mysql" {
			insert = `INSERT INTO entity_mapping_entities(scope,entity_key,token) VALUES(?,?,?) ON DUPLICATE KEY UPDATE entity_key=VALUES(entity_key)`
		}
		if _, err = tx.ExecContext(ctx, insert, scope, key, m.Token); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Storage) copyEntityMappings(ctx context.Context, source *sql.DB, opts CopyOptions, report *CopyReport) error {
	_, exists, err := tableColumns(ctx, source, opts.SourceMode, "entity_mapping_tokens")
	if err != nil || !exists {
		return err
	}
	// Import canonical records first, then reverse aliases. Never copy the
	// migration marker: imported legacy events must also be backfilled.
	rows, err := source.QueryContext(ctx, `SELECT t.scope,t.token,t.original,t.label,t.ambiguous FROM entity_mapping_tokens t LEFT JOIN entity_mapping_entities e ON t.scope=e.scope AND t.entity_key=e.entity_key AND t.token=e.token ORDER BY CASE WHEN e.token IS NULL THEN 1 ELSE 0 END,t.scope,t.token`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var scope string
		var m EntityMapping
		var original []byte
		var ambiguous int
		if err = rows.Scan(&scope, &m.Token, &original, &m.Label, &ambiguous); err != nil {
			return err
		}
		m.Original = string(original)
		if err = s.importEntityMappingState(ctx, scope, m, ambiguous != 0); err != nil {
			return err
		}
		report.EntityMappingsCopied++
	}
	return rows.Err()
}

func (s *Storage) verifyCopiedEntityMappings(ctx context.Context, source *sql.DB, report *CopyReport) error {
	if report.EntityMappingsCopied == 0 {
		return nil
	}
	rows, err := source.QueryContext(ctx, `SELECT t.scope,t.token,t.original,t.label,t.ambiguous,e.token FROM entity_mapping_tokens t LEFT JOIN entity_mapping_entities e ON t.scope=e.scope AND t.entity_key=e.entity_key AND t.token=e.token`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var scope, token, label string
		var original []byte
		var flag int
		var canonical sql.NullString
		if err = rows.Scan(&scope, &token, &original, &label, &flag, &canonical); err != nil {
			return err
		}
		var got []byte
		var gotLabel string
		var gotFlag int
		if err = s.db.QueryRowContext(ctx, `SELECT original,label,ambiguous FROM entity_mapping_tokens WHERE scope=? AND token=?`, scope, token).Scan(&got, &gotLabel, &gotFlag); err != nil {
			return err
		}
		if !bytes.Equal(got, original) || label != gotLabel || flag != gotFlag {
			continue
		}
		if canonical.Valid {
			var current string
			if err = s.db.QueryRowContext(ctx, `SELECT token FROM entity_mapping_entities WHERE scope=? AND entity_key=?`, scope, entityKey(string(original), label)).Scan(&current); errors.Is(err, sql.ErrNoRows) {
				continue
			} else if err != nil {
				return err
			}
			if current != canonical.String {
				continue
			}
		}
		report.EntityMappingsVerified++
	}
	return rows.Err()
}
