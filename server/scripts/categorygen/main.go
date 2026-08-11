// Command categorygen derives the authoritative category lookups from
// openapi.yaml. Every schema that pins both a `type` const and a `category`
// const contributes an event-type entry, and every operation contributes an
// entry from its `x-telemetry-category`. openapi.yaml is the single source of
// truth; this generator just surfaces it to Go.
//
// The api_call event reports the handler name oapi-codegen generated, not the
// spec's operationId, so operation entries are keyed by that name. The names
// are read out of the generated ServerInterface rather than derived from the
// operationId: each of its methods documents the route it serves, which joins
// to the spec on method and path with nothing assumed about how oapi-codegen
// spells a name. Run this after oapi-codegen, which `make oapi-generate` does.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type property struct {
	Const string `yaml:"const"`
}

type schema struct {
	Properties map[string]property `yaml:"properties"`
}

type operation struct {
	OperationID string `yaml:"operationId"`
	Category    string `yaml:"x-telemetry-category"`
}

// apiCallCategories are the categories an operation can be classified into.
// Only these two have an api_call event type to carry them, so any other value
// would name a category nothing publishes.
var apiCallCategories = map[string]struct{}{
	"control":  {},
	"platform": {},
}

// httpMethods are the path-item keys that describe an operation. Anything else
// under a path (a shared `parameters` list, a description) is not one.
var httpMethods = map[string]struct{}{
	"get": {}, "put": {}, "post": {}, "delete": {},
	"options": {}, "head": {}, "patch": {}, "trace": {},
}

// routeComment matches the route each generated ServerInterface method
// documents, e.g. "// (GET /process/{process_id}/status)".
var routeComment = regexp.MustCompile(`^//\s*\(([A-Z]+) (\S+)\)$`)

// handlerNamesByRoute reads the generated ServerInterface and returns the Go
// handler name for each route it serves, keyed as "METHOD /path".
func handlerNamesByRoute(path string) (map[string]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name.Name != "ServerInterface" {
				continue
			}
			iface, ok := ts.Type.(*ast.InterfaceType)
			if !ok {
				continue
			}
			for _, method := range iface.Methods.List {
				if len(method.Names) != 1 || method.Doc == nil {
					continue
				}
				for _, comment := range method.Doc.List {
					if m := routeComment.FindStringSubmatch(strings.TrimSpace(comment.Text)); m != nil {
						out[m[1]+" "+m[2]] = method.Names[0].Name
					}
				}
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no routes found on ServerInterface in %s", path)
	}
	return out, nil
}

type document struct {
	Paths      map[string]map[string]operation `yaml:"paths"`
	Components struct {
		Schemas map[string]schema `yaml:"schemas"`
	} `yaml:"components"`
}

func main() {
	openapiPath := flag.String("openapi", "", "path to openapi.yaml")
	handlersPath := flag.String("handlers", "", "path to the oapi-codegen output holding ServerInterface")
	outPath := flag.String("out", "", "path to the generated Go file")
	flag.Parse()
	if *openapiPath == "" || *handlersPath == "" || *outPath == "" {
		fmt.Fprintln(os.Stderr, "categorygen: -openapi, -handlers and -out are required")
		os.Exit(2)
	}

	handlers, err := handlerNamesByRoute(*handlersPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "categorygen: read handlers: %v\n", err)
		os.Exit(1)
	}

	raw, err := os.ReadFile(*openapiPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "categorygen: read openapi: %v\n", err)
		os.Exit(1)
	}
	var doc document
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		fmt.Fprintf(os.Stderr, "categorygen: parse openapi: %v\n", err)
		os.Exit(1)
	}

	type entry struct{ typ, category string }
	entries := make([]entry, 0, len(doc.Components.Schemas))
	for _, s := range doc.Components.Schemas {
		typ := s.Properties["type"].Const
		category := s.Properties["category"].Const
		if typ != "" && category != "" {
			entries = append(entries, entry{typ, category})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].typ < entries[j].typ })

	type opEntry struct{ handler, category string }
	ops := make([]opEntry, 0, len(handlers))
	for path, item := range doc.Paths {
		for method, op := range item {
			if _, isOperation := httpMethods[strings.ToLower(method)]; !isOperation {
				continue
			}
			route := strings.ToUpper(method) + " " + path
			if op.Category == "" {
				fmt.Fprintf(os.Stderr, "categorygen: %s (%s) has no x-telemetry-category\n", route, op.OperationID)
				os.Exit(1)
			}
			if _, ok := apiCallCategories[op.Category]; !ok {
				fmt.Fprintf(os.Stderr, "categorygen: %s (%s) has x-telemetry-category %q; an operation must be control or platform\n", route, op.OperationID, op.Category)
				os.Exit(1)
			}
			handler, ok := handlers[route]
			if !ok {
				fmt.Fprintf(os.Stderr, "categorygen: %s (%s) has no generated handler; re-run oapi-codegen\n", route, op.OperationID)
				os.Exit(1)
			}
			ops = append(ops, opEntry{handler, op.Category})
		}
	}
	if len(ops) != len(handlers) {
		fmt.Fprintf(os.Stderr, "categorygen: classified %d operations but ServerInterface serves %d routes\n", len(ops), len(handlers))
		os.Exit(1)
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].handler < ops[j].handler })

	var buf bytes.Buffer
	buf.WriteString("// Code generated by scripts/categorygen; DO NOT EDIT.\n\n")
	buf.WriteString("package events\n\n")
	buf.WriteString(`import oapi "github.com/kernel/kernel-images/server/lib/oapi"` + "\n\n")
	buf.WriteString("var categoryByType = map[string]oapi.TelemetryEventCategory{\n")
	for _, e := range entries {
		fmt.Fprintf(&buf, "\t%q: oapi.TelemetryEventCategory(%q),\n", e.typ, e.category)
	}
	buf.WriteString("}\n\n")
	buf.WriteString("// CategoryForType returns the authoritative category for a known event\n")
	buf.WriteString("// type. ok is false for an unknown type.\n")
	buf.WriteString("func CategoryForType(eventType string) (oapi.TelemetryEventCategory, bool) {\n")
	buf.WriteString("\tc, ok := categoryByType[eventType]\n")
	buf.WriteString("\treturn c, ok\n")
	buf.WriteString("}\n\n")
	buf.WriteString("var categoryByOperationID = map[string]oapi.TelemetryEventCategory{\n")
	for _, e := range ops {
		fmt.Fprintf(&buf, "\t%q: oapi.TelemetryEventCategory(%q),\n", e.handler, e.category)
	}
	buf.WriteString("}\n\n")
	buf.WriteString("// CategoryForOperation returns the category an api_call event carries for\n")
	buf.WriteString("// the given operation, keyed by the generated handler name the event\n")
	buf.WriteString("// reports. ok is false for an unknown operation.\n")
	buf.WriteString("func CategoryForOperation(operationID string) (oapi.TelemetryEventCategory, bool) {\n")
	buf.WriteString("\tc, ok := categoryByOperationID[operationID]\n")
	buf.WriteString("\treturn c, ok\n")
	buf.WriteString("}\n")

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		fmt.Fprintf(os.Stderr, "categorygen: format: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*outPath, formatted, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "categorygen: write %s: %v\n", *outPath, err)
		os.Exit(1)
	}
}
