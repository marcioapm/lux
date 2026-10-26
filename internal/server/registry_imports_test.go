package server

import (
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// registryNeutralFiles deliver and mint registry credentials for any
// provider; only a RegistryTokenSource (registry_ecr.go) may know AWS.
var registryNeutralFiles = []string{"registry.go", "hub.go", "server.go"}

// The delivery and minter code compiles without the AWS SDK: none of its
// files imports github.com/aws/....
func TestRegistryCoreImportsNoAWS(t *testing.T) {
	for _, name := range registryNeutralFiles {
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if path == "github.com/aws" || strings.HasPrefix(path, "github.com/aws/") {
				t.Errorf("%s imports %s: provider code belongs behind RegistryTokenSource", name, path)
			}
		}
	}
}
