package database

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrations embed.FS

type DB struct {
	Pool *pgxpool.Pool
}

func Open(ctx context.Context, databaseURL string, log *slog.Logger) (*DB, error) {
	if databaseURL == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		pool.Close()
		return nil, err
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		pool.Close()
		return nil, err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtext('parallax-schema-migrations'))`); err != nil {
		pool.Close()
		return nil, err
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext('parallax-schema-migrations'))`)
	sqlDB, err := sql.Open("pgx", databaseURL)
	if err != nil {
		pool.Close()
		return nil, err
	}
	defer sqlDB.Close()
	if err := goose.UpContext(ctx, sqlDB, "migrations"); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate postgres: %w", err)
	}
	if log != nil {
		log.Info("postgres ready")
	}
	return &DB{Pool: pool}, nil
}

func (d *DB) Close() {
	if d != nil && d.Pool != nil {
		d.Pool.Close()
	}
}

func (d *DB) Ready(ctx context.Context) error {
	if d == nil || d.Pool == nil {
		return errors.New("postgres is not configured")
	}
	return d.Pool.Ping(ctx)
}
