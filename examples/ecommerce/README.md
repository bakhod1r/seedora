# A schema with every shape in it

Twenty-three tables: a catalogue, people, orders laid over both, and the
support and audit tables that grow around them.

```sh
sqlite3 shop.db < schema.sql
seedora --dsn ./shop.db --config ./seedora.yaml
```

Or, from a clone of this repository:

```sh
make dev-big
```

## Why this schema

Every relationship shape is here on purpose, because each is drawn differently
and each is a way to get a diagram wrong.

| Shape | Where |
|---|---|
| One to many | `users` → `orders`, and most of the rest |
| One to one | `users` → `user_profiles`, whose key is also its foreign key |
| Many to many | `products` ↔ `tags`, through `product_tags` |
| Self reference | `categories.parent_id` → `categories.id` |
| Two keys into one parent | `orders` has a billing and a shipping address |

`seedora.yaml` is the mapping for it: row counts that keep the proportions
sensible (more order items than orders, more orders than users), and the
generators for the columns whose names do not say what they hold.

## Postgres

The same thing, with the types spelled the way Postgres spells them:

```sh
createdb shop_dev
seedora --dsn postgres://localhost:5432/shop_dev
```

Import `schema.sql` from the UI's **Import** dialog: its tables arrive as
drafts, and the SQL that would create them is shown before anything runs.
