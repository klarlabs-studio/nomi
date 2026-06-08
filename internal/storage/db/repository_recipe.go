package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// RecipeRow models a row in the recipes table. The full Recipe document
// is stored as YAML in `yaml`; the surrounding columns are denormalised
// for cheap listing.
type RecipeRow struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	Author      string    `json:"author,omitempty"`
	Description string    `json:"description,omitempty"`
	Tags        []string  `json:"tags,omitempty"`
	YAML        string    `json:"yaml"`
	SHA256      string    `json:"sha256"`
	Source      string    `json:"source"` // "imported" | "exported"
	CreatedAt   time.Time `json:"created_at"`
}

// RecipeRepository persists Recipe rows.
type RecipeRepository struct {
	db *DB
}

// NewRecipeRepository constructs a RecipeRepository.
func NewRecipeRepository(db *DB) *RecipeRepository {
	return &RecipeRepository{db: db}
}

// Upsert inserts a Recipe or replaces the existing row if the ID
// already exists. Treated as the same "version" of the recipe — callers
// that want side-by-side versions should namespace the ID.
func (r *RecipeRepository) Upsert(row *RecipeRow) error {
	tags, err := json.Marshal(row.Tags)
	if err != nil {
		return fmt.Errorf("marshal tags: %w", err)
	}
	source := row.Source
	if source == "" {
		source = "imported"
	}
	createdAt := row.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	_, err = r.db.Exec(`
		INSERT INTO recipes (id, name, version, author, description, tags, yaml, sha256, source, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		    name = excluded.name,
		    version = excluded.version,
		    author = excluded.author,
		    description = excluded.description,
		    tags = excluded.tags,
		    yaml = excluded.yaml,
		    sha256 = excluded.sha256,
		    source = excluded.source
	`,
		row.ID, row.Name, row.Version, row.Author, row.Description,
		tags, row.YAML, row.SHA256, source, createdAt,
	)
	if err != nil {
		return fmt.Errorf("upsert recipe: %w", err)
	}
	return nil
}

// GetByID returns the row for a recipe ID. Returns an error wrapping
// sql.ErrNoRows when the ID is not present.
func (r *RecipeRepository) GetByID(id string) (*RecipeRow, error) {
	return scanRecipe(r.db.QueryRow(`
		SELECT id, name, version, author, description, tags, yaml, sha256, source, created_at
		FROM recipes WHERE id = ?
	`, id))
}

// List returns every recipe row ordered by created_at desc.
func (r *RecipeRepository) List() ([]*RecipeRow, error) {
	rows, err := r.db.Query(`
		SELECT id, name, version, author, description, tags, yaml, sha256, source, created_at
		FROM recipes ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("list recipes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []*RecipeRow{}
	for rows.Next() {
		row, err := scanRecipe(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// Delete removes a recipe row.
func (r *RecipeRepository) Delete(id string) error {
	_, err := r.db.Exec(`DELETE FROM recipes WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete recipe: %w", err)
	}
	return nil
}

func scanRecipe(row rowScanner) (*RecipeRow, error) {
	var (
		out         RecipeRow
		author      sql.NullString
		description sql.NullString
		tags        []byte
	)
	err := row.Scan(&out.ID, &out.Name, &out.Version, &author, &description, &tags, &out.YAML, &out.SHA256, &out.Source, &out.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("recipe not found")
	}
	if err != nil {
		return nil, fmt.Errorf("scan recipe: %w", err)
	}
	if author.Valid {
		out.Author = author.String
	}
	if description.Valid {
		out.Description = description.String
	}
	if len(tags) > 0 {
		_ = json.Unmarshal(tags, &out.Tags)
	}
	return &out, nil
}
