package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"

	. "github.com/dave/jennifer/jen"
)

var knownImports = map[string]string{
	"result": "github.com/JustinKnueppel/go-result",
}

const InputFilePath = "./modules/protocol/protocol.go"
const InputStructName = "Protocol"

const OutputFilePath = "./modules/manager/manager_gen.go"
const OutputStructName = "manager"

func main() {
	fset := token.NewFileSet() // positions are relative to fset

	f := NewFile("manager")

	f.HeaderComment("This is an auto-generated file. DO NOT EDIT!\n" +
		"To modify generation, update /scripts/manager_gen/manager_gen.go\n" +
		"To modify protocol interface, update /modules/protocol/protocol.go")

	for importName, importPath := range knownImports {
		f.ImportAlias(importPath, importName)
	}

	// Parse src but stop after processing the imports.
	fp, err := parser.ParseFile(fset, InputFilePath, nil, parser.AllErrors)
	if err != nil {
		fmt.Println(err)
		return
	}

	done := false

	for _, dec := range fp.Decls {
		if done {
			break
		}
		dec, ok := dec.(*ast.GenDecl)
		if !ok {
			continue
		}
		if dec.Tok != token.TYPE {
			continue
		}
		// fmt.Printf("declaration: %+v\n", dec.Tok)
		for _, spec := range dec.Specs {
			if done {
				break
			}
			spec, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			name := spec.Name.Name
			if name != InputStructName {
				continue
			}
			// typeParams := spec.TypeParams
			// fmt.Printf("  spec: %+v\n", spec.Type)
			Type, ok := spec.Type.(*ast.StructType)
			if !ok {
				continue
			}
			if Type.Incomplete {
				fmt.Println("syntax error")
				return
			}
			for _, field := range Type.Fields.List {
				// fmt.Printf("    field: %+v\n", field)
				// fmt.Printf("      type: %+v\n", field.Type)
				Type, ok := field.Type.(*ast.FuncType)
				if !ok {
					continue
				}
				var results []*ast.Field
				if Type.Results != nil {
					results = Type.Results.List
				}
				fnCall := Id("protocol").Dot(field.Names[0].Name).Call(GenerateCallArgs(Type.Params.List)...)
				execBlock := []Code{Return(fnCall)}
				if len(results) == 0 {
					execBlock = []Code{fnCall, Return()}
				}
				WithReturnType(f.Func().Params(
					Id("m").Id("*"+OutputStructName),
				).Id(field.Names[0].Name).Params(
					GenerateParams(Type.Params.List)...,
				), results).Block(
					append([]Code{
						For(
							Id("i").Op(":=").Id("m").Dot("version"),
							Id("i").Op(">=").Lit(0),
							Id("i").Op("--"),
						).Block(
							Id("protocol").Op(":=").Id("m").Dot("protocols").Index(Id("i")),
							If(Id("protocol").Dot(field.Names[0].Name).Op("!=").Id("nil")).Block(
								execBlock...,
							),
						),
					},
						append(
							GenerateDefaultValues(results),
							Return(GenerateDefaultReturnValue(results)...),
						)...,
					)...,
				)
			}
			done = true
			break
		}
	}

	file, err := os.Create(OutputFilePath)
	if err != nil {
		fmt.Println("err:", err)
		return
	}

	fmt.Fprintf(file, "%#v", f)
}

func GenerateDefaultReturnValue(fields []*ast.Field) (res []Code) {
	for i, field := range fields {
		if len(field.Names) > 0 {
			res = append(res, Id(field.Names[0].Name))
		} else {
			res = append(res, Id(fmt.Sprintf("res%d", i)))
		}
	}
	return
}

func GenerateDefaultValues(fields []*ast.Field) (res []Code) {
	for i, field := range fields {
		var stmt *Statement
		if len(field.Names) > 0 {
			stmt = Var().Id(field.Names[0].Name)
		} else {
			stmt = Var().Id(fmt.Sprintf("res%d", i))
		}
		stmt.Add(generateType(field.Type))
		res = append(res, stmt)
	}
	return
}

func generateType(expr ast.Expr) *Statement {
	switch Type := expr.(type) {
	case *ast.Ident:
		return Id(Type.Name)
	case *ast.IndexExpr:
		return generateType(Type.X).Index(generateType(Type.Index))
	case *ast.IndexListExpr:
		params := make([]Code, len(Type.Indices))
		for i, indexType := range Type.Indices {
			params[i] = generateType((indexType))
		}
		return generateType(Type.X).Index(params...)
	case *ast.SelectorExpr:
		id, ok := Type.X.(*ast.Ident)
		if !ok {
			return generateType(Type.X).Dot(Type.Sel.Name)
		}
		name, ok := knownImports[id.Name]
		if !ok {
			name = id.Name
		}
		return Qual(name, Type.Sel.Name)
	default:
		fmt.Printf("type not supported %+v\n", expr)
		return new(Statement)
	}
}

func WithReturnType(s *Statement, fields []*ast.Field) *Statement {
	if len(fields) == 0 {
		return s
	}
	var res []Code
	for _, field := range fields {
		res = append(res, generateType(field.Type))
	}
	return s.Params(res...)
}

func GenerateParams(fields []*ast.Field) (res []Code) {
	for i, field := range fields {
		var stmt *Statement
		if len(field.Names) > 0 {
			stmt = Id(field.Names[0].Name)
		} else {
			stmt = Id(fmt.Sprintf("arg%d", i))
		}
		stmt.Add(generateType(field.Type))
		res = append(res, stmt)
	}
	return
}

func GenerateCallArgs(fields []*ast.Field) (res []Code) {
	for i, field := range fields {
		if len(field.Names) > 0 {
			res = append(res, Id(field.Names[0].Name))
		} else {
			res = append(res, Id(fmt.Sprintf("arg%d", i)))
		}
	}
	return
}
