//go:build tools

// this file's name, structure, and placement is from:
// - https://github.com/99designs/gqlgen

package tools

import (
	_ "github.com/99designs/gqlgen"
	_ "github.com/99designs/gqlgen/graphql/introspection"
)
