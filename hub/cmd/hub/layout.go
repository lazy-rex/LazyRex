package main

import (
	"context"
	"database/sql"
	"time"
)

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// lnode is one node of a Rex window layout (session.view): a block or a split.
type lnode struct {
	BlockID string  `json:"block_id,omitempty"`
	Split   *lsplit `json:"split,omitempty"`
}

type lsplit struct {
	Direction string  `json:"direction"`
	Ratio     float64 `json:"ratio"`
	Before    *lnode  `json:"before"`
	After     *lnode  `json:"after"`
}

func (n *lnode) leaves(out []string) []string {
	if n == nil {
		return out
	}
	if n.Split != nil {
		return n.Split.After.leaves(n.Split.Before.leaves(out))
	}
	return append(out, n.BlockID)
}
