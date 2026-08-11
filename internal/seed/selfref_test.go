package seed_test

import (
	"testing"

	"github.com/bakhod1r/seedora/internal/plan"
	"github.com/bakhod1r/seedora/internal/seed"
)

// A table that references itself, and a second table that references it.
//
// This is the shape the large demo schema has — categories with a parent_id,
// and products pointing at categories — and it is worth its own test because
// the self-reference is resolved before the table it points at has any rows,
// which is a state no other table is ever in.
const selfRefSchema = `
CREATE TABLE categories (
  id        INTEGER PRIMARY KEY,
  parent_id INTEGER REFERENCES categories(id),
  name      VARCHAR(80) NOT NULL
);

CREATE TABLE products (
  id          INTEGER PRIMARY KEY,
  category_id INTEGER NOT NULL REFERENCES categories(id),
  name        VARCHAR(80) NOT NULL
);
`

// A child of a self-referencing table must get real keys.
//
// The bug this covers: the pool of parent keys is cached per reference so a
// parent read once serves every child. The self-reference resolves
// categories.id while categories is still empty, and caching that empty result
// handed the same empty pool to products, whose category_id is NOT NULL — so
// the run died on a constraint two tables later, naming a column whose plan was
// correct.
func TestAChildOfASelfReferencingTableGetsRealKeys(t *testing.T) {
	d, s, raw := openWith(t, selfRefSchema)

	p := plan.Infer(s)
	p.Tables["categories"].Rows = 30
	p.Tables["products"].Rows = 100

	if _, err := seed.Run(t.Context(), d, s, p, seed.Options{Seed: 12, Batch: 16}); err != nil {
		t.Fatal(err)
	}

	var products, orphans int
	if err := raw.QueryRow(`SELECT
		(SELECT COUNT(*) FROM products),
		(SELECT COUNT(*) FROM products p
		   LEFT JOIN categories c ON c.id = p.category_id WHERE c.id IS NULL)`).
		Scan(&products, &orphans); err != nil {
		t.Fatal(err)
	}
	if products != 100 {
		t.Errorf("wrote %d products, want 100", products)
	}
	if orphans != 0 {
		t.Errorf("%d products point at no category", orphans)
	}
}

// A foreign key that is also the primary key is a one-to-one, and each parent
// may be used once.
//
// The constraint is real but invisible where the seeder was looking for it: a
// single-column primary key has no unique index of its own — on SQLite it is
// the rowid — so nothing marked the column unique, the one-to-one assigner
// declined the table, and the ordinary uniqueness repair replaced a duplicate
// with the row index, which on a foreign key is a value pointing at nothing.
const oneToOneSchema = `
CREATE TABLE users (
  id    INTEGER PRIMARY KEY,
  email VARCHAR(60) NOT NULL UNIQUE
);

CREATE TABLE user_profiles (
  user_id INTEGER PRIMARY KEY REFERENCES users(id),
  locale  VARCHAR(10) NOT NULL
);
`

func TestAPrimaryKeyThatIsAForeignKeyIsAOneToOne(t *testing.T) {
	d, s, raw := openWith(t, oneToOneSchema)

	p := plan.Infer(s)
	p.Tables["users"].Rows = 60
	p.Tables["user_profiles"].Rows = 60

	if _, err := seed.Run(t.Context(), d, s, p, seed.Options{Seed: 8, Batch: 16}); err != nil {
		t.Fatal(err)
	}

	var profiles, distinct, orphans int
	if err := raw.QueryRow(`SELECT
		(SELECT COUNT(*) FROM user_profiles),
		(SELECT COUNT(DISTINCT user_id) FROM user_profiles),
		(SELECT COUNT(*) FROM user_profiles p
		   LEFT JOIN users u ON u.id = p.user_id WHERE u.id IS NULL)`).
		Scan(&profiles, &distinct, &orphans); err != nil {
		t.Fatal(err)
	}
	if profiles != 60 || distinct != 60 {
		t.Errorf("%d profiles over %d distinct users, want 60 and 60", profiles, distinct)
	}
	if orphans != 0 {
		t.Errorf("%d profiles point at no user", orphans)
	}
}

// The self-reference itself is nullable and has nothing to point at when it is
// resolved, so it is NULL — but it must not be an error, and it must not
// prevent the table from being seeded.
func TestASelfReferenceIsNotAnError(t *testing.T) {
	d, s, raw := openWith(t, selfRefSchema)

	p := plan.Infer(s)
	p.Tables["categories"].Rows = 20
	p.Tables["products"].Rows = 0

	if _, err := seed.Run(t.Context(), d, s, p, seed.Options{Seed: 3, Batch: 8}); err != nil {
		t.Fatal(err)
	}

	var categories, dangling int
	if err := raw.QueryRow(`SELECT
		(SELECT COUNT(*) FROM categories),
		(SELECT COUNT(*) FROM categories c
		   WHERE c.parent_id IS NOT NULL
		     AND NOT EXISTS (SELECT 1 FROM categories p WHERE p.id = c.parent_id))`).
		Scan(&categories, &dangling); err != nil {
		t.Fatal(err)
	}
	if categories != 20 {
		t.Errorf("wrote %d categories, want 20", categories)
	}
	if dangling != 0 {
		t.Errorf("%d categories point at a parent that does not exist", dangling)
	}
}
