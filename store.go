package main

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store keeps TUS variables and data. owner is a core account UUID, or "v:<name>" for a
// title's virtual user. setter is the JID of whoever wrote the slot last.
type Store struct{ DB *pgxpool.Pool }

type Variable struct {
	Slot    int
	Value   int64
	Setter  string
	Updated time.Time
}

type Data struct {
	Slot    int
	Info    []byte
	Body    []byte
	Size    int64
	Setter  string
	Updated time.Time
}

func (s *Store) Migrate(ctx context.Context) error {
	_, err := s.DB.Exec(ctx, `
	CREATE TABLE IF NOT EXISTS tus_variables (
		commid  text        NOT NULL,
		owner   text        NOT NULL,
		slot    integer     NOT NULL,
		value   bigint      NOT NULL,
		setter  text        NOT NULL,
		updated timestamptz NOT NULL,
		PRIMARY KEY (commid, owner, slot));
	CREATE TABLE IF NOT EXISTS tus_data (
		commid  text        NOT NULL,
		owner   text        NOT NULL,
		slot    integer     NOT NULL,
		info    bytea       NOT NULL,
		body    bytea       NOT NULL,
		setter  text        NOT NULL,
		updated timestamptz NOT NULL,
		PRIMARY KEY (commid, owner, slot))`)
	return err
}

// Variables returns the stored slots of one owner; missing slots are absent from the map.
func (s *Store) Variables(ctx context.Context, commid, owner string, slots []int) (map[int]Variable, error) {
	rows, err := s.DB.Query(ctx, `SELECT slot, value, setter, updated FROM tus_variables
		WHERE commid = $1 AND owner = $2 AND slot = ANY($3)`, commid, owner, slots)
	if err != nil {
		return nil, err
	}
	out := map[int]Variable{}
	for rows.Next() {
		var v Variable
		if err := rows.Scan(&v.Slot, &v.Value, &v.Setter, &v.Updated); err != nil {
			return nil, err
		}
		out[v.Slot] = v
	}
	return out, rows.Err()
}

func (s *Store) SetVariables(ctx context.Context, commid, owner, setter string, vars []Variable) error {
	batch := &pgx.Batch{}
	for _, v := range vars {
		batch.Queue(`INSERT INTO tus_variables VALUES ($1, $2, $3, $4, $5, now())
			ON CONFLICT (commid, owner, slot) DO UPDATE SET value = $4, setter = $5, updated = now()`,
			commid, owner, v.Slot, v.Value, setter)
	}
	return s.DB.SendBatch(ctx, batch).Close()
}

// AddVariable adds delta to a slot (missing counts as 0) and returns the new row.
func (s *Store) AddVariable(ctx context.Context, commid, owner, setter string, slot int, delta int64) (Variable, error) {
	v := Variable{Slot: slot}
	err := s.DB.QueryRow(ctx, `INSERT INTO tus_variables VALUES ($1, $2, $3, $4, $5, now())
		ON CONFLICT (commid, owner, slot) DO UPDATE SET value = tus_variables.value + $4, setter = $5, updated = now()
		RETURNING value, setter, updated`, commid, owner, slot, delta, setter).Scan(&v.Value, &v.Setter, &v.Updated)
	return v, err
}

// Update runs fn on the current slot (nil when missing) under a row lock; fn returns the
// value to store, or false to leave the slot unchanged.
func (s *Store) Update(ctx context.Context, commid, owner, setter string, slot int, fn func(*Variable) (int64, bool)) (cur *Variable, err error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	v := Variable{Slot: slot}
	err = tx.QueryRow(ctx, `SELECT value, setter, updated FROM tus_variables
		WHERE commid = $1 AND owner = $2 AND slot = $3 FOR UPDATE`, commid, owner, slot).Scan(&v.Value, &v.Setter, &v.Updated)
	if err == nil {
		cur = &v
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	value, ok := fn(cur)
	if !ok {
		return cur, nil
	}
	next := Variable{Slot: slot}
	if err = tx.QueryRow(ctx, `INSERT INTO tus_variables VALUES ($1, $2, $3, $4, $5, now())
		ON CONFLICT (commid, owner, slot) DO UPDATE SET value = $4, setter = $5, updated = now()
		RETURNING value, setter, updated`, commid, owner, slot, value, setter).Scan(&next.Value, &next.Setter, &next.Updated); err != nil {
		return nil, err
	}
	return &next, tx.Commit(ctx)
}

func (s *Store) DeleteVariables(ctx context.Context, commid, owner string, slots []int) error {
	_, err := s.DB.Exec(ctx, `DELETE FROM tus_variables WHERE commid = $1 AND owner = $2 AND slot = ANY($3)`, commid, owner, slots)
	return err
}

// DataStatus returns slot metadata without the bodies.
func (s *Store) DataStatus(ctx context.Context, commid, owner string, slots []int) (map[int]Data, error) {
	rows, err := s.DB.Query(ctx, `SELECT slot, info, length(body), setter, updated FROM tus_data
		WHERE commid = $1 AND owner = $2 AND slot = ANY($3)`, commid, owner, slots)
	if err != nil {
		return nil, err
	}
	out := map[int]Data{}
	for rows.Next() {
		var d Data
		if err := rows.Scan(&d.Slot, &d.Info, &d.Size, &d.Setter, &d.Updated); err != nil {
			return nil, err
		}
		out[d.Slot] = d
	}
	return out, rows.Err()
}

func (s *Store) GetData(ctx context.Context, commid, owner string, slot int) (*Data, error) {
	d := Data{Slot: slot}
	err := s.DB.QueryRow(ctx, `SELECT info, body, setter, updated FROM tus_data
		WHERE commid = $1 AND owner = $2 AND slot = $3`, commid, owner, slot).Scan(&d.Info, &d.Body, &d.Setter, &d.Updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	d.Size = int64(len(d.Body))
	return &d, err
}

func (s *Store) SetData(ctx context.Context, commid, owner, setter string, slot int, info, body []byte) error {
	_, err := s.DB.Exec(ctx, `INSERT INTO tus_data VALUES ($1, $2, $3, $4, $5, $6, now())
		ON CONFLICT (commid, owner, slot) DO UPDATE SET info = $4, body = $5, setter = $6, updated = now()`,
		commid, owner, slot, info, body, setter)
	return err
}

func (s *Store) DeleteData(ctx context.Context, commid, owner string, slots []int) error {
	_, err := s.DB.Exec(ctx, `DELETE FROM tus_data WHERE commid = $1 AND owner = $2 AND slot = ANY($3)`, commid, owner, slots)
	return err
}
