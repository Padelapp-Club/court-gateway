package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Padelapp-Club/court-gateway/internal/core"
)

// dbTimeout bounds every call to the database: one that does not answer
// makes the hub answer 503 (ErrStateUnavailable), it does not hang it.
const dbTimeout = 5 * time.Second

// pgSchema holds the hub's tables. The hub makes them when they are
// missing, at every start, and never drops or changes one.
const pgSchema = "court_gateway_hub"

var pgTables = []string{
	`CREATE SCHEMA IF NOT EXISTS ` + pgSchema,
	`CREATE TABLE IF NOT EXISTS ` + pgSchema + `.installations (
		id      text PRIMARY KEY,
		name    text NOT NULL,
		created timestamptz NOT NULL,
		version bigint NOT NULL CHECK (version >= 0)
	)`,
	// json, not jsonb: the spec comes back byte for byte as the hub wrote it
	`CREATE TABLE IF NOT EXISTS ` + pgSchema + `.clubs (
		id           text PRIMARY KEY,
		installation text NOT NULL REFERENCES ` + pgSchema + `.installations (id) ON DELETE CASCADE,
		spec         json NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS clubs_installation ON ` + pgSchema + `.clubs (installation)`,
	`CREATE TABLE IF NOT EXISTS ` + pgSchema + `.gateways (
		installation  text PRIMARY KEY REFERENCES ` + pgSchema + `.installations (id) ON DELETE CASCADE,
		name          text NOT NULL,
		secret_sha256 text NOT NULL UNIQUE CHECK (secret_sha256 ~ '^[0-9a-f]{64}$'),
		registered_at timestamptz NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS ` + pgSchema + `.join_codes (
		code_sha256  text PRIMARY KEY CHECK (code_sha256 ~ '^[0-9a-f]{64}$'),
		installation text NOT NULL,
		name         text NOT NULL,
		created      timestamptz NOT NULL,
		expires      timestamptz NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS join_codes_installation ON ` + pgSchema + `.join_codes (installation)`,
}

type pgState struct{ pool *pgxpool.Pool }

// openPostgres connects to the database of url and makes the hub's tables
// in it when they are missing.
func openPostgres(url string) (State, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, errors.New("database_url_env: строка подключения к Postgres не разбирается")
	}
	if cfg.ConnConfig.ConnectTimeout == 0 || cfg.ConnConfig.ConnectTimeout > dbTimeout {
		cfg.ConnConfig.ConnectTimeout = dbTimeout
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return nil, fmt.Errorf("Postgres: %w", err)
	}
	p := pgState{pool: pool}
	err = p.tx(func(ctx context.Context, tx pgx.Tx) error {
		// two processes starting at once (the hub and the CLI) make the
		// tables one after the other
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('`+pgSchema+`'))`); err != nil {
			return err
		}
		for _, q := range pgTables {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("Postgres: схема %s: %w", pgSchema, err)
	}
	return p, nil
}

// failed is a database error as the store reports it: a refusal of the
// data (a broken constraint, ErrClubTaken) as it is, any
// other — no connection, a timeout, a server going down — as
// ErrStateUnavailable.
func failed(err error) error {
	var pe *pgconn.PgError
	switch {
	case err == nil, errors.Is(err, ErrClubTaken):
		return err
	case errors.As(err, &pe) && (strings.HasPrefix(pe.Code, "22") || strings.HasPrefix(pe.Code, "23")):
		return err
	}
	return fmt.Errorf("%w: %v", ErrStateUnavailable, err)
}

// tx runs f in a transaction within dbTimeout.
func (p pgState) tx(f func(context.Context, pgx.Tx) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error { return f(ctx, tx) })
}

// exec runs one statement within dbTimeout.
func (p pgState) exec(q string, args ...any) (pgconn.CommandTag, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	tag, err := p.pool.Exec(ctx, q, args...)
	return tag, failed(err)
}

func (p pgState) load() ([]*Installation, []GatewayRecord, error) {
	var installs []*Installation
	var gateways []GatewayRecord
	err := p.tx(func(ctx context.Context, tx pgx.Tx) error {
		byID := map[string]*Installation{}
		rows, _ := tx.Query(ctx, `SELECT id, name, created, version FROM `+pgSchema+`.installations ORDER BY id`)
		for rows.Next() {
			in := &Installation{Clubs: map[string]core.ClubSpec{}}
			if err := rows.Scan(&in.ID, &in.Name, &in.Created, &in.Version); err != nil {
				return err
			}
			if !core.ValidID(in.ID) {
				return fmt.Errorf("повреждённая запись установки %q", in.ID)
			}
			in.Created = in.Created.UTC()
			installs = append(installs, in)
			byID[in.ID] = in
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows, _ = tx.Query(ctx, `SELECT installation, id, spec FROM `+pgSchema+`.clubs ORDER BY installation, id`)
		for rows.Next() {
			var inst, id string
			var spec []byte
			if err := rows.Scan(&inst, &id, &spec); err != nil {
				return err
			}
			var c core.ClubSpec
			if err := json.Unmarshal(spec, &c); err != nil || c.ID != id {
				return fmt.Errorf("повреждённая запись клуба %q", id)
			}
			byID[inst].Clubs[id] = c
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows, _ = tx.Query(ctx, `SELECT installation, name, secret_sha256, registered_at FROM `+pgSchema+`.gateways ORDER BY installation`)
		for rows.Next() {
			var g GatewayRecord
			if err := rows.Scan(&g.Installation, &g.Name, &g.SecretSHA256, &g.RegisteredAt); err != nil {
				return err
			}
			g.RegisteredAt = g.RegisteredAt.UTC()
			gateways = append(gateways, g)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, nil, fmt.Errorf("Postgres: %w", err)
	}
	return installs, gateways, nil
}

func (p pgState) putCode(hash string, rec joinCodeRecord) error {
	_, err := p.exec(`INSERT INTO `+pgSchema+`.join_codes (code_sha256, installation, name, created, expires) VALUES ($1, $2, $3, $4, $5)`,
		hash, rec.Installation, rec.Name, rec.Created, rec.Expires)
	return err
}

// takeCode: one statement deletes the row and returns it, so of two
// takes of one code only one finds it.
func (p pgState) takeCode(hash string) (joinCodeRecord, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	var rec joinCodeRecord
	err := p.pool.QueryRow(ctx, `DELETE FROM `+pgSchema+`.join_codes WHERE code_sha256 = $1 RETURNING installation, name, created, expires`, hash).
		Scan(&rec.Installation, &rec.Name, &rec.Created, &rec.Expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return joinCodeRecord{}, ErrJoinCode
	}
	return rec, failed(err)
}

func (p pgState) putGateway(g *GatewayRecord) error {
	_, err := p.exec(`INSERT INTO `+pgSchema+`.gateways (installation, name, secret_sha256, registered_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (installation) DO UPDATE SET name = excluded.name, secret_sha256 = excluded.secret_sha256, registered_at = excluded.registered_at`,
		g.Installation, g.Name, g.SecretSHA256, g.RegisteredAt)
	return err
}

func (p pgState) hasGateway(installation string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	var there bool
	err := p.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+pgSchema+`.gateways WHERE installation = $1)`, installation).Scan(&there)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrStateUnavailable, err)
	}
	return there, nil
}

func (p pgState) gateways() (map[string]bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	rows, _ := p.pool.Query(ctx, `SELECT installation FROM `+pgSchema+`.gateways`)
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStateUnavailable, err)
	}
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

func (p pgState) deleteGateway(installation string) error {
	tag, err := p.exec(`DELETE FROM `+pgSchema+`.gateways WHERE installation = $1`, installation)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotRegistered
	}
	return err
}

// putInstallation writes in and its clubs at once. The version only
// grows: when the row already holds in.Version or more (a commit whose
// answer was lost, so the hub did not learn of it), in.Version is raised
// above it.
func (p pgState) putInstallation(in *Installation) error {
	return failed(p.tx(func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO `+pgSchema+`.installations (id, name, created, version) VALUES ($1, $2, $3, $4)
			ON CONFLICT (id) DO UPDATE SET name = excluded.name, version = GREATEST(installations.version + 1, excluded.version)
			RETURNING version`, in.ID, in.Name, in.Created, in.Version).Scan(&in.Version)
		if err != nil {
			return err
		}
		ids := make([]string, 0, len(in.Clubs))
		for id := range in.Clubs {
			ids = append(ids, id)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM `+pgSchema+`.clubs WHERE installation = $1 AND NOT (id = ANY ($2))`, in.ID, ids); err != nil {
			return err
		}
		for _, c := range in.ClubList() {
			spec, err := json.Marshal(c)
			if err != nil {
				return err
			}
			tag, err := tx.Exec(ctx, `INSERT INTO `+pgSchema+`.clubs (id, installation, spec) VALUES ($1, $2, $3)
				ON CONFLICT (id) DO UPDATE SET spec = excluded.spec WHERE clubs.installation = excluded.installation`, c.ID, in.ID, string(spec))
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return ErrClubTaken
			}
		}
		return nil
	}))
}

func (p pgState) deleteInstallation(id string) error {
	return failed(p.tx(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM `+pgSchema+`.join_codes WHERE installation = $1`, id); err != nil {
			return err
		}
		// its clubs and gateway go with it (ON DELETE CASCADE)
		_, err := tx.Exec(ctx, `DELETE FROM `+pgSchema+`.installations WHERE id = $1`, id)
		return err
	}))
}

func (p pgState) ready() error {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	if err := p.pool.Ping(ctx); err != nil {
		return fmt.Errorf("%w: %v", ErrStateUnavailable, err)
	}
	return nil
}

func (p pgState) Close() { p.pool.Close() }
