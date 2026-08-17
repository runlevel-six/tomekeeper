// Package store is the data access layer.
//
// # Scoping
//
// Principle 2.8 of the implementation plan: user-scoped data is scoped
// structurally, not by convention. Two mechanisms enforce that here.
//
// First, UserID is a distinct type rather than an int64. A method that needs a
// user cannot be called with a feed id, an article id, or a bare literal that
// happened to be in scope — those are compile errors, not silent leaks.
//
// Second, the handful of operations that legitimately cross users — the
// scheduler asking "which feeds are due" — do not live on Store at all. They
// live on SystemStore, reachable only through Store.System(), and are
// documented as unreachable from a request handler. The exception is therefore
// greppable: `grep -r '\.System()'` lists every place user scoping is
// deliberately not applied.
package store

import (
	"github.com/jackc/pgx/v5/pgxpool"
)

// UserID identifies a user. It is a distinct type so that scoping cannot be
// satisfied by any other integer in scope.
type UserID int64

// FeedID identifies a feed.
type FeedID int64

// ArticleID identifies an article. Articles are a global pool shared by all
// users, so no method taking one is user-scoped by virtue of the article.
type ArticleID int64

// Store is the user-scoped data access layer. Every method that touches
// user-owned data takes a UserID.
type Store struct {
	pool *pgxpool.Pool
}

// New returns a Store over the given pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Pool exposes the underlying connection pool for components that manage
// their own transactions, such as the River client.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// System returns the cross-user operations.
//
// Everything reachable here ignores user scoping by design. It is for
// background workers only. Nothing on SystemStore may be called from an HTTP
// handler, and nothing on it may return another user's content to a request.
func (s *Store) System() *SystemStore { return &SystemStore{pool: s.pool} }

// SystemStore holds operations that deliberately span users.
type SystemStore struct {
	pool *pgxpool.Pool
}
