package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Skill is one row in `skills`. Visibility, latest_version pointer, and yanked_at
// live with the metadata; archives themselves live on disk and are tracked in
// SkillVersion rows.
type Skill struct {
	ID            string     `json:"id"`
	Slug          string     `json:"slug"`
	DisplayName   string     `json:"display_name"`
	Description   string     `json:"description"`
	Visibility    string     `json:"visibility"`
	LatestVersion string     `json:"latest_version,omitempty"`
	YankedAt      *time.Time `json:"yanked_at,omitempty"`
	CreatedBy     *string    `json:"created_by,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// SkillVersion is one row in `skill_versions`. Each row corresponds to a single
// uploaded archive at <bundles_dir>/<archive_path>. SHA-256 lets the client
// verify integrity after download.
type SkillVersion struct {
	SkillID       string          `json:"skill_id"`
	Version       string          `json:"version"`
	ArchivePath   string          `json:"archive_path"`
	ArchiveSize   int64           `json:"archive_size"`
	ArchiveSHA256 string          `json:"archive_sha256"`
	Manifest      json.RawMessage `json:"manifest"`
	YankedAt      *time.Time      `json:"yanked_at,omitempty"`
	UploadedBy    *string         `json:"uploaded_by,omitempty"`
	UploadedAt    time.Time       `json:"uploaded_at"`
}

// SkillAssignment grants a user access to a restricted skill. A NULL Version
// means "follow latest" — i.e. the user always gets whatever Skill.LatestVersion
// points to at sync time.
type SkillAssignment struct {
	SkillID    string    `json:"skill_id"`
	UserID     string    `json:"user_id"`
	Version    *string   `json:"version,omitempty"`
	AssignedBy *string   `json:"assigned_by,omitempty"`
	AssignedAt time.Time `json:"assigned_at"`
}

// ErrSkillSlugConflict is returned when a slug is already taken.
var ErrSkillSlugConflict = errors.New("skill slug already exists")

// ErrSkillVersionConflict is returned when (skill_id, version) is already taken.
var ErrSkillVersionConflict = errors.New("skill version already exists")

// SkillStore is the persistence layer for skills, versions, and assignments.
type SkillStore struct {
	db *DB
}

// NewSkillStore returns a SkillStore backed by db.
func NewSkillStore(db *DB) *SkillStore {
	return &SkillStore{db: db}
}

// CreateSkill inserts a new skill row. Reuses the slug regex from servers.go
// via ValidateSlug so all relay-managed slugs share one rule.
func (s *SkillStore) CreateSkill(sk *Skill) error {
	if err := ValidateSlug(sk.Slug); err != nil {
		return err
	}
	if sk.ID == "" {
		sk.ID = uuid.New().String()
	}
	if sk.Visibility == "" {
		sk.Visibility = "restricted"
	}
	if sk.Visibility != "public" && sk.Visibility != "restricted" {
		return fmt.Errorf("invalid visibility %q", sk.Visibility)
	}
	now := time.Now()
	sk.CreatedAt = now
	sk.UpdatedAt = now

	_, err := s.db.Exec(`
		INSERT INTO skills (id, slug, display_name, description, visibility, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sk.ID, sk.Slug, sk.DisplayName, sk.Description, sk.Visibility, sk.CreatedBy, sk.CreatedAt, sk.UpdatedAt,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrSkillSlugConflict
		}
		return fmt.Errorf("creating skill: %w", err)
	}
	return nil
}

// GetSkill returns a skill by id, or (nil, nil) if not found.
func (s *SkillStore) GetSkill(id string) (*Skill, error) {
	return s.scanSkillRow(s.db.QueryRow(`
		SELECT id, slug, display_name, description, visibility,
		       COALESCE(latest_version, ''), yanked_at, created_by, created_at, updated_at
		FROM skills WHERE id = ?`, id))
}

// GetSkillBySlug returns a skill by slug, or (nil, nil) if not found.
func (s *SkillStore) GetSkillBySlug(slug string) (*Skill, error) {
	return s.scanSkillRow(s.db.QueryRow(`
		SELECT id, slug, display_name, description, visibility,
		       COALESCE(latest_version, ''), yanked_at, created_by, created_at, updated_at
		FROM skills WHERE slug = ?`, slug))
}

// ListSkills returns all skills ordered by slug. Yanked skills are included —
// callers filter as needed; admin views want them, public listings don't.
func (s *SkillStore) ListSkills() ([]*Skill, error) {
	rows, err := s.db.Query(`
		SELECT id, slug, display_name, description, visibility,
		       COALESCE(latest_version, ''), yanked_at, created_by, created_at, updated_at
		FROM skills ORDER BY slug`)
	if err != nil {
		return nil, fmt.Errorf("listing skills: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*Skill
	for rows.Next() {
		sk, err := s.scanSkill(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sk)
	}
	return out, rows.Err()
}

// UpdateSkillMeta patches the mutable metadata fields. Slug, ID, and
// latest_version are managed elsewhere (slug rename via separate API in Phase 1
// if ever; latest_version is set as a side effect of CreateVersion).
func (s *SkillStore) UpdateSkillMeta(id, displayName, description, visibility string) error {
	if visibility != "" && visibility != "public" && visibility != "restricted" {
		return fmt.Errorf("invalid visibility %q", visibility)
	}
	_, err := s.db.Exec(`
		UPDATE skills SET display_name = ?, description = ?, visibility = COALESCE(NULLIF(?, ''), visibility),
		                  updated_at = ?
		WHERE id = ?`,
		displayName, description, visibility, time.Now(), id,
	)
	if err != nil {
		return fmt.Errorf("updating skill meta: %w", err)
	}
	return nil
}

// YankSkill marks a skill as yanked at now(). Yanked skills are hidden from
// public listings and `assigned` queries but the row + its archives are kept,
// so previously-installed clients keep working until they sync next.
func (s *SkillStore) YankSkill(id string) error {
	now := time.Now()
	_, err := s.db.Exec(`UPDATE skills SET yanked_at = ?, updated_at = ? WHERE id = ?`, now, now, id)
	if err != nil {
		return fmt.Errorf("yanking skill: %w", err)
	}
	return nil
}

// UnyankSkill clears yanked_at, returning the skill to active listings.
func (s *SkillStore) UnyankSkill(id string) error {
	_, err := s.db.Exec(`UPDATE skills SET yanked_at = NULL, updated_at = ? WHERE id = ?`, time.Now(), id)
	if err != nil {
		return fmt.Errorf("unyanking skill: %w", err)
	}
	return nil
}

// DeleteSkill removes a skill row. ON DELETE CASCADE cleans up versions and
// assignments. Disk archives are NOT removed by this — the caller is expected
// to do that after a successful delete (or schedule it for cleanup). Hard
// delete should be reserved for "never published" mistakes; prefer YankSkill.
func (s *SkillStore) DeleteSkill(id string) error {
	_, err := s.db.Exec(`DELETE FROM skills WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting skill: %w", err)
	}
	return nil
}

// CreateVersion inserts a version row and atomically advances skills.latest_version
// to the new version. The atomic update keeps "latest" pointing at the most
// recently uploaded version regardless of semver ordering — uploaders are
// expected to push monotonically increasing versions; if they don't, the latest
// pointer reflects upload order, which is the intuitive behavior for a fleet
// rollout (the most recent push is the one operators want clients to pick up).
func (s *SkillStore) CreateVersion(v *SkillVersion) error {
	if v.Version == "" {
		return fmt.Errorf("version is required")
	}
	if v.ArchivePath == "" {
		return fmt.Errorf("archive_path is required")
	}
	if v.ArchiveSize <= 0 {
		return fmt.Errorf("archive_size must be > 0")
	}
	if v.ArchiveSHA256 == "" {
		return fmt.Errorf("archive_sha256 is required")
	}
	if len(v.Manifest) == 0 {
		v.Manifest = json.RawMessage("{}")
	}
	v.UploadedAt = time.Now()

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.Exec(`
		INSERT INTO skill_versions
		    (skill_id, version, archive_path, archive_size, archive_sha256, manifest, uploaded_by, uploaded_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		v.SkillID, v.Version, v.ArchivePath, v.ArchiveSize, v.ArchiveSHA256, string(v.Manifest), v.UploadedBy, v.UploadedAt,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrSkillVersionConflict
		}
		return fmt.Errorf("inserting skill version: %w", err)
	}

	if _, err := tx.Exec(`UPDATE skills SET latest_version = ?, updated_at = ? WHERE id = ?`,
		v.Version, v.UploadedAt, v.SkillID); err != nil {
		return fmt.Errorf("updating latest_version: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

// GetVersion returns one version row, or (nil, nil) if not found.
func (s *SkillStore) GetVersion(skillID, version string) (*SkillVersion, error) {
	v := &SkillVersion{}
	var manifest string
	var yankedAt sql.NullTime
	err := s.db.QueryRow(`
		SELECT skill_id, version, archive_path, archive_size, archive_sha256, manifest,
		       yanked_at, uploaded_by, uploaded_at
		FROM skill_versions WHERE skill_id = ? AND version = ?`, skillID, version,
	).Scan(&v.SkillID, &v.Version, &v.ArchivePath, &v.ArchiveSize, &v.ArchiveSHA256, &manifest,
		&yankedAt, &v.UploadedBy, &v.UploadedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting skill version: %w", err)
	}
	v.Manifest = json.RawMessage(manifest)
	if yankedAt.Valid {
		t := yankedAt.Time
		v.YankedAt = &t
	}
	return v, nil
}

// ListVersions returns all versions for a skill, newest upload first.
func (s *SkillStore) ListVersions(skillID string) ([]*SkillVersion, error) {
	rows, err := s.db.Query(`
		SELECT skill_id, version, archive_path, archive_size, archive_sha256, manifest,
		       yanked_at, uploaded_by, uploaded_at
		FROM skill_versions WHERE skill_id = ? ORDER BY uploaded_at DESC`, skillID)
	if err != nil {
		return nil, fmt.Errorf("listing skill versions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*SkillVersion
	for rows.Next() {
		v := &SkillVersion{}
		var manifest string
		var yankedAt sql.NullTime
		if err := rows.Scan(&v.SkillID, &v.Version, &v.ArchivePath, &v.ArchiveSize, &v.ArchiveSHA256,
			&manifest, &yankedAt, &v.UploadedBy, &v.UploadedAt); err != nil {
			return nil, fmt.Errorf("scanning skill version: %w", err)
		}
		v.Manifest = json.RawMessage(manifest)
		if yankedAt.Valid {
			t := yankedAt.Time
			v.YankedAt = &t
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// YankVersion marks a single version as yanked. Existing clients can still
// download by exact `@version` pin (Phase 1 will surface this as a flag);
// "follow latest" sync skips yanked versions.
func (s *SkillStore) YankVersion(skillID, version string) error {
	now := time.Now()
	res, err := s.db.Exec(`UPDATE skill_versions SET yanked_at = ? WHERE skill_id = ? AND version = ?`,
		now, skillID, version)
	if err != nil {
		return fmt.Errorf("yanking skill version: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// UnyankVersion clears yanked_at on a version.
func (s *SkillStore) UnyankVersion(skillID, version string) error {
	res, err := s.db.Exec(`UPDATE skill_versions SET yanked_at = NULL WHERE skill_id = ? AND version = ?`,
		skillID, version)
	if err != nil {
		return fmt.Errorf("unyanking skill version: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// AssignSkill grants a user access to a restricted skill. version may be empty
// to mean "follow latest". Calling AssignSkill on an existing assignment is an
// upsert — the version pin is replaced.
func (s *SkillStore) AssignSkill(a *SkillAssignment) error {
	if a.SkillID == "" || a.UserID == "" {
		return fmt.Errorf("skill_id and user_id are required")
	}
	a.AssignedAt = time.Now()
	_, err := s.db.Exec(`
		INSERT INTO skill_assignments (skill_id, user_id, version, assigned_by, assigned_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(skill_id, user_id) DO UPDATE SET
		    version = excluded.version,
		    assigned_by = excluded.assigned_by,
		    assigned_at = excluded.assigned_at`,
		a.SkillID, a.UserID, a.Version, a.AssignedBy, a.AssignedAt,
	)
	if err != nil {
		return fmt.Errorf("assigning skill: %w", err)
	}
	return nil
}

// UnassignSkill revokes a single user's access to a restricted skill.
func (s *SkillStore) UnassignSkill(skillID, userID string) error {
	_, err := s.db.Exec(`DELETE FROM skill_assignments WHERE skill_id = ? AND user_id = ?`, skillID, userID)
	if err != nil {
		return fmt.Errorf("unassigning skill: %w", err)
	}
	return nil
}

// ListAssignmentsForSkill returns who has been granted access to a given skill.
func (s *SkillStore) ListAssignmentsForSkill(skillID string) ([]*SkillAssignment, error) {
	rows, err := s.db.Query(`
		SELECT skill_id, user_id, version, assigned_by, assigned_at
		FROM skill_assignments WHERE skill_id = ? ORDER BY assigned_at`, skillID)
	if err != nil {
		return nil, fmt.Errorf("listing assignments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanAssignments(rows)
}

// AssignedSkill is the row shape returned by AssignedForUser — joins skill
// metadata onto the assignment so the client gets everything it needs in one
// call (slug, latest_version, version pin if any).
type AssignedSkill struct {
	Skill         *Skill  `json:"skill"`
	PinnedVersion *string `json:"pinned_version,omitempty"`
}

// AssignedForUser returns every skill the user has access to right now —
// public-and-not-yanked plus restricted skills with an explicit assignment.
// Used by `arc-sync skill sync` to compute the desired client state.
func (s *SkillStore) AssignedForUser(userID string) ([]*AssignedSkill, error) {
	rows, err := s.db.Query(`
		SELECT s.id, s.slug, s.display_name, s.description, s.visibility,
		       COALESCE(s.latest_version, ''), s.yanked_at, s.created_by, s.created_at, s.updated_at,
		       a.version
		FROM skills s
		LEFT JOIN skill_assignments a
		    ON a.skill_id = s.id AND a.user_id = ?
		WHERE s.yanked_at IS NULL
		  AND (s.visibility = 'public' OR a.user_id IS NOT NULL)
		ORDER BY s.slug`, userID)
	if err != nil {
		return nil, fmt.Errorf("listing assigned skills: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*AssignedSkill
	for rows.Next() {
		sk := &Skill{}
		var yankedAt sql.NullTime
		var pinned sql.NullString
		if err := rows.Scan(&sk.ID, &sk.Slug, &sk.DisplayName, &sk.Description, &sk.Visibility,
			&sk.LatestVersion, &yankedAt, &sk.CreatedBy, &sk.CreatedAt, &sk.UpdatedAt, &pinned); err != nil {
			return nil, fmt.Errorf("scanning assigned skill: %w", err)
		}
		if yankedAt.Valid {
			t := yankedAt.Time
			sk.YankedAt = &t
		}
		as := &AssignedSkill{Skill: sk}
		if pinned.Valid {
			v := pinned.String
			as.PinnedVersion = &v
		}
		out = append(out, as)
	}
	return out, rows.Err()
}

// scanSkill reads one row off rows.Scan-compatible iterator into a Skill.
func (s *SkillStore) scanSkill(scanner interface {
	Scan(dest ...any) error
}) (*Skill, error) {
	sk := &Skill{}
	var yankedAt sql.NullTime
	if err := scanner.Scan(&sk.ID, &sk.Slug, &sk.DisplayName, &sk.Description, &sk.Visibility,
		&sk.LatestVersion, &yankedAt, &sk.CreatedBy, &sk.CreatedAt, &sk.UpdatedAt); err != nil {
		return nil, fmt.Errorf("scanning skill: %w", err)
	}
	if yankedAt.Valid {
		t := yankedAt.Time
		sk.YankedAt = &t
	}
	return sk, nil
}

func (s *SkillStore) scanSkillRow(row *sql.Row) (*Skill, error) {
	sk, err := s.scanSkill(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return sk, err
}

func scanAssignments(rows *sql.Rows) ([]*SkillAssignment, error) {
	var out []*SkillAssignment
	for rows.Next() {
		a := &SkillAssignment{}
		var v sql.NullString
		if err := rows.Scan(&a.SkillID, &a.UserID, &v, &a.AssignedBy, &a.AssignedAt); err != nil {
			return nil, fmt.Errorf("scanning skill assignment: %w", err)
		}
		if v.Valid {
			s := v.String
			a.Version = &s
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
