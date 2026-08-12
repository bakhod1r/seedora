//go:build nosql || all

// Cheap in bytes, expensive in concepts. Every driver here is one to four
// megabytes, so size is not what keeps them out of the default build: it is
// that Seedora infers what data should look like from a catalog of tables and
// columns, and these engines have varying amounts of one.
//
// Cassandra and Elasticsearch have a real, typed, queryable schema and behave
// almost like the relational engines. Neo4j and DynamoDB have part of one.
// MongoDB has none, so its driver infers columns by sampling documents, which
// is inference from data rather than a declared schema. Redis has nothing at
// all. Each driver says which of those it is doing.

package main

import (
	_ "github.com/bakhod1r/seedora/internal/db/cassandra"
	_ "github.com/bakhod1r/seedora/internal/db/dynamodb"
	_ "github.com/bakhod1r/seedora/internal/db/elasticsearch"
	_ "github.com/bakhod1r/seedora/internal/db/mongodb"
	_ "github.com/bakhod1r/seedora/internal/db/neo4j"
	_ "github.com/bakhod1r/seedora/internal/db/redis"
)
