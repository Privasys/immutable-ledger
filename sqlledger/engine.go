// Copyright (c) Privasys. All rights reserved.
// Licensed under the GNU Affero General Public License v3.0. See LICENSE file for details.

package sqlledger

import (
	"context"

	sqle "github.com/dolthub/go-mysql-server"
	"github.com/dolthub/go-mysql-server/sql"
	"github.com/dolthub/go-mysql-server/sql/information_schema"
)

// The engine is embedded in-process only: the application links the
// adapter and runs statements through it. There is deliberately no
// network listener — on this platform the application is the policy
// boundary for its data, and SQL is an internal implementation tool
// behind it, not a query surface of its own.

// Provider is the sql.DatabaseProvider for one store.
type Provider struct {
	db   *Database
	info sql.Database
}

var _ sql.DatabaseProvider = (*Provider)(nil)

// NewProvider builds a provider serving the store's database plus
// information_schema.
func NewProvider(s *Store) *Provider {
	return &Provider{
		db:   NewDatabase(s),
		info: information_schema.NewInformationSchemaDatabase(),
	}
}

func (p *Provider) Database(_ *sql.Context, name string) (sql.Database, error) {
	switch {
	case equalsFold(name, p.db.Name()):
		return p.db, nil
	case equalsFold(name, p.info.Name()):
		return p.info, nil
	default:
		return nil, sql.ErrDatabaseNotFound.New(name)
	}
}

func (p *Provider) HasDatabase(_ *sql.Context, name string) bool {
	return equalsFold(name, p.db.Name()) || equalsFold(name, p.info.Name())
}

func (p *Provider) AllDatabases(*sql.Context) []sql.Database {
	return []sql.Database{p.db, p.info}
}

func equalsFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// Engine bundles the query engine with its store.
type Engine struct {
	*sqle.Engine
	store *Store
}

// NewEngine builds an in-process MySQL-dialect engine over the store.
func NewEngine(s *Store) *Engine {
	return &Engine{Engine: sqle.NewDefault(NewProvider(s)), store: s}
}

// Store returns the underlying SQL store.
func (e *Engine) Store() *Store { return e.store }

// NewContext returns a fresh session context with the store's database
// selected. Sessions are cheap; use one per goroutine.
func (e *Engine) NewContext(ctx context.Context) *sql.Context {
	sctx := sql.NewContext(ctx, sql.WithSession(sql.NewBaseSession()))
	sctx.SetCurrentDatabase(e.store.dbName)
	return sctx
}

// Exec runs one statement and returns its materialised rows (nil for
// statements without a result set beyond status).
func (e *Engine) Exec(ctx *sql.Context, statement string) ([]sql.Row, error) {
	_, iter, _, err := e.Query(ctx, statement)
	if err != nil {
		return nil, err
	}
	return sql.RowIterToRows(ctx, iter)
}
